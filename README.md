# eze

Drop in a folder of HTML and get back a private URL that only people in your organisation can open.
No frameworks, pipelines or config files. Need data? Every site gets a database, realtime updates, file uploads,
multiplayer rooms and the viewer's identity through a tiny browser SDK, with nothing to set up.

An open-source take on the idea behind Shopify's internal [Quick](https://shopify.engineering/quick) platform.

```sh
eze login https://eze.page
eze init lunch-vote          # starter page + AGENTS.md so your coding agent knows the SDK
cd lunch-vote
claude "make a site where my team can vote on lunch spots in real time"
eze deploy                   # live at https://lunch-vote.eze.page
```

## Why it is this simple

Everything sits behind your company's sign-in, so the hard parts of the open web disappear:
no spam, no abuse, no API keys in the browser. eze leans into that:

- **A site is a folder.** Deploying uploads a tarball and atomically swaps the directory.
- **No permissions.** Any signed-in person can deploy, overwrite or delete any site
  (set `EZE_OVERWRITE=owner` if you want first-deployer ownership). Every deploy records who did it.
- **A small, fixed set of capabilities.** Static files plus the SDK. No custom backends, no cron.
  The constraints are the feature.
- **One binary, one SQLite file.** Runs comfortably on a small VM.

## The browser SDK

```html
<script src="/_q/sdk.js"></script>
<script>
  const me = await eze.id.me();                       // { email, name }
  const posts = eze.db.collection('posts');
  await posts.create({ title: 'Hello', status: 'draft' });
  const drafts = await posts.find({ status: 'draft' });
  posts.subscribe({ onCreate: (doc) => console.log('new', doc) });
  const photo = await eze.files.upload(input.files[0]);  // { id, name, url, size, ... }
  const room = eze.ws.join('lobby');                     // multiplayer: messages + presence
  room.on('presence', (peer) => drawCursor(peer.user.name, peer.state));
  document.onmousemove = (e) => room.setState({ x: e.clientX, y: e.clientY });
</script>
```

The full reference is what `eze init` writes into [AGENTS.md](internal/cli/templates/AGENTS.md).
Each site's data is isolated to that site. API calls are same-origin, authenticated by the
viewer's existing session, and rate limited per user.

## Run it

You need a domain with a wildcard DNS record (`eze.page` and `*.eze.page`
pointing at the server) and a wildcard TLS certificate, typically from a reverse proxy like Caddy.

eze **refuses to start without authentication**. Pick one:

| `EZE_AUTH` | How people sign in |
|---|---|
| `oidc` | Built-in OpenID Connect login (Google Workspace, Okta, Entra ID, Keycloak...). One session cookie covers every site. |
| `header` | An auth proxy in front (oauth2-proxy, Pomerium, Cloudflare Access, Tailscale) sets an email header. Only honoured from `EZE_TRUSTED_PROXIES`. |
| `dev` | Everyone is `EZE_DEV_EMAIL`. Only allowed on a loopback address, for local development. |

See [docs/deploy.md](docs/deploy.md) for complete setups.

### Configuration

| Variable | Default | |
|---|---|---|
| `EZE_DOMAIN` | required | Apex host. Sites are served at `<site>.<EZE_DOMAIN>`. |
| `EZE_AUTH` | required | `oidc`, `header` or `dev`. |
| `EZE_ADDR` | `:8080` | Listen address. |
| `EZE_SCHEME` | `https` | Public scheme, used for links, redirects and cookies. |
| `EZE_DATA_DIR` | `./data` | Deployed sites, uploads and `eze.db`. Back this up. |
| `EZE_OVERWRITE` | `anyone` | `anyone` or `owner` (first deployer owns the site). |
| `EZE_OIDC_ISSUER`, `EZE_OIDC_CLIENT_ID`, `EZE_OIDC_CLIENT_SECRET` | | OIDC client. Redirect URI: `https://<EZE_DOMAIN>/_q/auth/callback`. |
| `EZE_OIDC_ALLOWED_DOMAINS` | | Comma-separated email domains allowed to sign in. Required for OIDC. |
| `EZE_SESSION_SECRET` | | Base64, at least 32 bytes (`openssl rand -base64 32`). Required for OIDC. |
| `EZE_AUTH_EMAIL_HEADER` | `X-Forwarded-Email` | Header mode: email header. |
| `EZE_AUTH_NAME_HEADER` | `X-Forwarded-User` | Header mode: display name header. |
| `EZE_TRUSTED_PROXIES` | `127.0.0.1/32,::1/128` | Header mode: CIDRs allowed to set identity headers. |
| `EZE_MAX_DEPLOY_BYTES` | 50 MB | Max compressed deploy archive. |
| `EZE_MAX_SITE_BYTES` | 200 MB | Max uncompressed site. |
| `EZE_MAX_SITE_FILES` | 10000 | Max files per site. |
| `EZE_MAX_DOC_BYTES` | 1 MB | Max database document. |
| `EZE_MAX_UPLOAD_BYTES` | 25 MB | Max uploaded file. |
| `EZE_UPLOAD_QUOTA_BYTES` | 1 GB | Total uploads per site. |
| `EZE_RATE_LIMIT`, `EZE_RATE_BURST` | 20/s, 60 | Per-user API rate limit. |

### Try it locally

```sh
go build -o eze ./cmd/eze
EZE_DOMAIN=localhost:8080 EZE_SCHEME=http EZE_AUTH=dev EZE_ADDR=127.0.0.1:8080 ./eze serve
```

In another terminal (`*.localhost` resolves to your machine in Chrome, Firefox and curl):

```sh
./eze login http://localhost:8080
./eze init hello && cd hello && ../eze deploy
open http://hello.localhost:8080
```

## CLI

```
eze login <server-url>          sign the CLI in through your browser
eze init [dir] [--site name]    scaffold a site with agent instructions
eze deploy [dir] [--site name]  upload a folder; replaces the site atomically
eze ls                          list all sites
eze rm <site> [--yes]           delete a site, its files and its data
eze whoami                      show the signed-in user
```

The site name comes from `--site`, then `eze.json`, then the directory name. Hidden files,
`node_modules`, `AGENTS.md`, `CLAUDE.md` and `eze.json` are never uploaded. In CI, set
`EZE_SERVER` and `EZE_TOKEN` instead of running `eze login`.

## Roadmap

- AI proxy with server-held keys (`eze.ai`)

## License

MIT
