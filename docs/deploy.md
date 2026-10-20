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

## Backups

All state lives in `EZE_DATA_DIR`: `sites/` (deployed files), `uploads/` (files uploaded
through sites) and `eze.db` (SQLite, WAL mode). Back up with `sqlite3 eze.db ".backup backup.db"`
plus a copy of `sites/` and `uploads/`.

`eze.ai` replies stream as server-sent events. eze sends `X-Accel-Buffering: no` so nginx
won't buffer them; other proxies may need response buffering turned off for `/_q/ai/`.

If a reverse proxy limits request bodies (nginx defaults to 1 MB), raise it to at least
`EZE_MAX_UPLOAD_BYTES` and `EZE_MAX_DEPLOY_BYTES`, e.g. `client_max_body_size 64m;`.
