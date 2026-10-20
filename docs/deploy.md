# Deploying eze

Every setup needs:

1. **DNS:** `eze.example.com` and `*.eze.example.com` pointing at your server.
2. **TLS:** a certificate covering both names. Wildcards need a DNS challenge, which Caddy handles with a DNS plugin.
3. **Authentication:** built-in OIDC, or an auth proxy in front. eze will not start without one.

eze serves plain HTTP; put it behind a reverse proxy that terminates TLS and forwards the
original `Host` header (Caddy and nginx both do by default) and supports websockets.

## Option A: built-in OIDC + Caddy (simplest)

Register an OIDC client with your identity provider:

- Redirect URI: `https://eze.example.com/_q/auth/callback`
- Scopes: `openid profile email`

Caddyfile (using the [Cloudflare DNS plugin](https://github.com/caddy-dns/cloudflare) for the wildcard cert):

```caddy
eze.example.com, *.eze.example.com {
	tls {
		dns cloudflare {env.CF_API_TOKEN}
	}
	reverse_proxy 127.0.0.1:8080
}
```

eze:

```sh
EZE_DOMAIN=eze.example.com \
EZE_ADDR=127.0.0.1:8080 \
EZE_AUTH=oidc \
EZE_OIDC_ISSUER=https://accounts.google.com \
EZE_OIDC_CLIENT_ID=... \
EZE_OIDC_CLIENT_SECRET=... \
EZE_OIDC_ALLOWED_DOMAINS=example.com \
EZE_SESSION_SECRET="$(openssl rand -base64 32)" \
EZE_DATA_DIR=/var/lib/eze \
eze serve
```

Keep `EZE_SESSION_SECRET` stable across restarts, or everyone gets signed out.

## Option B: behind an auth proxy (header mode)

Use this when your company already puts internal tools behind oauth2-proxy, Pomerium,
Cloudflare Access or Tailscale. The proxy authenticates the user and sets an email header;
eze trusts that header **only** when the TCP connection comes from `EZE_TRUSTED_PROXIES`.
Make sure nothing else can reach eze's port directly.

The CLI authenticates with an eze API token (`Authorization: Bearer eze_...`), which your
proxy does not understand. Let requests to `/_q/api/` through without proxy auth. eze only
accepts API tokens on `/_q/api/`: identity headers and cookies are ignored there, so a client
can't sign itself in by sending its own `X-Forwarded-Email` on that path. Everything else,
including the dashboard, must stay behind the proxy.

### oauth2-proxy

```sh
oauth2-proxy \
  --upstream=http://127.0.0.1:8080 \
  --http-address=127.0.0.1:4180 \
  --reverse-proxy=true \
  --pass-user-headers=true \
  --cookie-domain=.eze.example.com \
  --whitelist-domain=.eze.example.com \
  --email-domain=example.com \
  --skip-auth-route='^/_q/api/' \
  --skip-auth-strip-headers=true \
  ...provider flags...
```

`--skip-auth-strip-headers` (the default) is important: it removes any `X-Forwarded-Email`
a client sends on the skipped routes, so it can't be spoofed through them.

eze:

```sh
EZE_DOMAIN=eze.example.com EZE_AUTH=header EZE_ADDR=127.0.0.1:8080 \
EZE_TRUSTED_PROXIES=127.0.0.1/32 eze serve
```

### Cloudflare Access

Set `EZE_AUTH_EMAIL_HEADER=Cf-Access-Authenticated-User-Email`. Add a second Access application
for the path `eze.example.com/_q/api/` with a Bypass policy so the CLI's token reaches eze.
Restrict `EZE_TRUSTED_PROXIES` to the address `cloudflared` connects from (usually `127.0.0.1/32`).

### Tailscale Serve

Set `EZE_AUTH_EMAIL_HEADER=Tailscale-User-Login` and `EZE_AUTH_NAME_HEADER=Tailscale-User-Name`.

## AI for sites (optional)

`eze.ai` is off until you configure a provider. Pick one:

```sh
# Claude
EZE_ANTHROPIC_API_KEY=sk-ant-...

# OpenAI
EZE_OPENAI_API_KEY=sk-... EZE_AI_MODELS=<model>,<another-model>

# Any OpenAI-compatible server, e.g. a model you host with Ollama or vLLM
EZE_OPENAI_BASE_URL=http://gpu-box:11434/v1 EZE_AI_MODELS=<model>
```

With an OpenAI-compatible server, check that it reports token usage on streamed replies
(`stream_options.include_usage`). If it doesn't, eze charges each reply its full reservation
against the person's budget, because it can't know the real count.

## Docker

```sh
docker build -t eze .
docker run -d -p 127.0.0.1:8080:8080 -v eze-data:/data \
  -e EZE_DOMAIN=eze.example.com -e EZE_AUTH=oidc ... eze
```

In header mode with Docker, connections arrive from the Docker bridge, so set
`EZE_TRUSTED_PROXIES` to the proxy container's address or network, never `0.0.0.0/0`.

## Storing files in a bucket

By default deployed sites and uploads live in `EZE_DATA_DIR`. To keep them in S3, an
S3-compatible store or Google Cloud Storage instead, set `EZE_STORAGE`:

```sh
EZE_STORAGE=s3://my-bucket?region=us-east-1
EZE_STORAGE=gs://my-bucket
# Cloudflare R2, Backblaze B2, Garage and other S3-compatible stores:
EZE_STORAGE='s3://my-bucket?endpoint=https://<account>.r2.cloudflarestorage.com&region=auto&use_path_style=true'
```

The `region` must be the one the store expects: `auto` for R2, the bucket's region for B2,
and `garage` (or your `s3_region`) for Garage. A wrong one fails at startup with an
authorization error.

Add `&prefix=eze/` to keep eze's files in one folder of a shared bucket. Credentials come
from the usual places: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, an instance role or
`~/.aws`, and for Google Cloud `GOOGLE_APPLICATION_CREDENTIALS` or workload identity. eze
checks at startup that it can write, read and delete in the bucket, and refuses to start if not.

eze needs these permissions on the bucket and nothing else:

- **S3:** `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket`.
- **Google Cloud Storage:** `roles/storage.objectAdmin` on the bucket.

Keep the bucket private: block public access and don't turn on website hosting. Visitors
never fetch from the bucket directly. eze streams every file itself after checking who is
signing in, which is the whole point.

`EZE_DATA_DIR` is still needed for `eze.db` and as scratch space for unpacking deploys.
eze runs as a single server either way: the database and live rooms are on that machine.

### Running your own: Garage

To keep files on your own servers, [Garage](https://garagehq.deuxfleurs.fr) is a light,
self-hosted S3-compatible store that fits eze well. A single node with Docker:

```toml
# /etc/garage.toml
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "<openssl rand -hex 32>"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
```

```sh
docker run -d --name garage --restart unless-stopped -p 127.0.0.1:3900:3900 \
  -v /etc/garage.toml:/etc/garage.toml -v /var/lib/garage:/var/lib/garage dxflrs/garage:v2.4.1

alias garage="docker exec garage /garage"
garage status                                   # note the node ID
garage layout assign -z dc1 -c 100G <node-id>
garage layout apply --version 1
garage bucket create eze
garage key create eze                           # prints the key ID and secret
garage bucket allow --read --write --owner eze --key eze
```

Then start eze with the key from `garage key create`:

```sh
AWS_ACCESS_KEY_ID=GK... AWS_SECRET_ACCESS_KEY=... \
EZE_STORAGE='s3://eze?endpoint=http://127.0.0.1:3900&region=garage&use_path_style=true' \
eze serve
```

Keep port 3900 private to the machine or network eze runs on. For more storage or
redundancy, add nodes and raise `replication_factor`; see Garage's documentation.

### Moving an existing install

Stop eze, then copy its sites and uploads into the bucket:

```sh
EZE_DATA_DIR=/var/lib/eze EZE_STORAGE=s3://my-bucket?region=us-east-1 eze storage migrate
```

It's safe to run again: sites and uploads already in the bucket are skipped (`--force`
re-copies sites). Start eze with `EZE_STORAGE` set, check your sites, then delete `sites/`
and `uploads/` from the data folder.

## Backups

`eze.db` (SQLite, WAL mode) always lives in `EZE_DATA_DIR`. Back it up with
`sqlite3 eze.db ".backup backup.db"`, or stream it continuously to a bucket with
[Litestream](https://litestream.io).

Without `EZE_STORAGE`, also back up `sites/` (deployed files) and `uploads/` (files uploaded
through sites) in the same folder. With a bucket, use the bucket's own versioning or backups.

`eze.ai` replies stream as server-sent events. eze sends `X-Accel-Buffering: no` so nginx
won't buffer them; other proxies may need response buffering turned off for `/_q/ai/`.

If a reverse proxy limits request bodies (nginx defaults to 1 MB), raise it to at least
`EZE_MAX_UPLOAD_BYTES` and `EZE_MAX_DEPLOY_BYTES`, e.g. `client_max_body_size 64m;`.
