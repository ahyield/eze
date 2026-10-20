# {{SITE}}

This directory is an **eze** site: a folder of static files (HTML, CSS, JS, images) that
`eze deploy` publishes to `https://{{SITE}}.<eze domain>/`. Only signed-in members of the
organisation can open it.

## Rules

- No build step is required. Plain HTML/CSS/JS in this folder is what gets served. If you do use a
  bundler, deploy its output folder: `eze deploy dist`.
- `index.html` at the top level is the home page. A top-level `404.html` is used for missing paths.
- Load libraries from a CDN (e.g. `https://cdn.jsdelivr.net/npm/...`) or vendor them into the folder.
- Hidden files, `node_modules`, `AGENTS.md`, `CLAUDE.md` and `eze.json` are never uploaded.
- There is no server-side code. Anything dynamic uses the browser SDK below.
- To publish, run `eze deploy` in this folder. Deploying replaces the whole site atomically.

## Browser SDK

Add this before your own scripts. It is served by eze itself, same origin, no keys needed:

```html
<script src="/_q/sdk.js"></script>
```

It defines `window.eze`.

### Identity

```js
const me = await eze.id.me(); // { email, name } of whoever is viewing the page
```

### Database

Schemaless JSON documents grouped in collections. Data is private to this site and shared by
everyone who uses it. Collection names and ids: letters, digits, `-` and `_`.

```js
const posts = eze.db.collection('posts');

const doc = await posts.create({ title: 'Hello', status: 'draft' }); // id generated
await posts.set('pinned', { title: 'Pinned' });  // create or replace with your own id
await posts.update(doc.id, { status: 'published' }); // shallow merge; null removes a field
const one = await posts.get(doc.id);                 // null if missing
await posts.delete(doc.id);

const all = await posts.list();                                  // oldest first, max 100
const recent = await posts.list({ order: 'desc', limit: 20 });   // limit up to 1000, plus offset
const drafts = await posts.find({ status: 'draft' });            // equality filters, ANDed
const byAuthor = await posts.find({ 'author.email': me.email }); // dotted paths for nested fields
```

Every document returned includes `id`, `_created_at`, `_updated_at` (ISO strings) and
`_created_by` (email). Fields starting with `_` are reserved and dropped on write.
Documents are limited to 1 MB. Filters support string, number, boolean and null values only;
sort or filter anything more complex in the browser.

### Realtime

```js
const unsubscribe = posts.subscribe({
  onCreate: (doc) => {},
  onUpdate: (doc) => {},
  onDelete: (id) => {},
  onReconnect: () => {}, // connection came back after a drop: refetch with list()
});
```

Use this for live updates to saved data (polls, boards, chat history). Load the initial state
with `list()`, then apply events. For live state that doesn't need saving, use rooms.

### Rooms (multiplayer)

For things that don't need saving: cursors, typing indicators, live reactions, game moves,
WebRTC signalling. Each browser tab is a peer; everyone's signed-in identity comes for free.

```js
const room = eze.ws.join('lobby', { state: { color: 'tomato' } }); // state is optional

room.on('ready', (peers) => {});          // connected (also after reconnects): full list, re-render from it
room.on('join', (peer) => {});            // peer: { id, user: { email, name }, state }
room.on('leave', (peer) => {});
room.on('presence', (peer) => {});        // someone's state changed
room.on('message', (data, peer) => {});

room.send({ emoji: '🎉' });               // to everyone else
room.sendTo(peer.id, { offer });          // to one peer
room.setState({ x, y });                  // your presence; safe to call on every mousemove
room.peers;                               // Map of the other peers by id
room.id;                                  // your own peer id
room.leave();
```

Messages are relayed live and never stored: anyone who joins later won't see earlier ones.
Keep durable things (scores, chat history) in the database, and use rooms for the live layer.
Limits: 200 peers per room, 64 KB per message, about 50 messages per second per tab (extra
messages are dropped, so throttle anything you send in a loop). The connection reconnects by
itself; your peer id changes when it does, and `ready` fires again.

### Files

Uploads are shared by everyone using the site and survive redeploys. Max 25 MB per file by default.

```js
const input = document.querySelector('input[type=file]');
const file = await eze.files.upload(input.files[0], {
  onProgress: (fraction) => {},       // optional, 0..1
});
// file: { id, name, size, content_type, url, _created_by, _created_at }
img.src = file.url;                   // use url directly in <img>, <video>, <a href>

const recent = await eze.files.list({ limit: 50 }); // newest first
const one = await eze.files.get(file.id);           // null if missing
await eze.files.delete(file.id);
```

`upload` also takes any Blob (e.g. from `canvas.toBlob`), with `{ name: 'drawing.png' }`.
Store `file.id` or `file.url` in a database document to attach files to records.
Images, video, audio, PDFs, plain text, CSV and JSON open in the browser; other types download.

### AI

Call Claude from the page. The server holds the API key; each person has a daily token
budget shared across all sites. Check `(await eze.ai.usage()).enabled` first if the feature is optional.

```js
const { text } = await eze.ai.chat('Summarize: ' + notes);

// Conversation, options, and streaming as it's written:
const res = await eze.ai.chat(
  [{ role: 'user', content: 'Hi' }, { role: 'assistant', content: 'Hello!' }, { role: 'user', content: 'Plan my week' }],
  {
    system: 'You are a concise planner.',    // optional
    effort: 'low',                           // optional: low | medium | high | xhigh | max
    onText: (chunk, soFar) => { out.textContent = soFar; },
    signal: controller.signal,               // optional AbortController signal
  },
);
// res: { text, model, stop_reason, usage: { input_tokens, output_tokens } }

const u = await eze.ai.usage(); // { enabled, used, limit, resets_at, models, default_model, max_tokens_cap }
```

Messages are text only. Stream (`onText`) anything a person waits on. Check `res.stop_reason`:
`'refusal'` means the model declined, `'max_tokens'` means the reply was cut off. A 429 error
means the person's budget can't cover the request, or they already have 3 AI calls running;
show the error message rather than retrying in a loop. Each call reserves its prompt size plus
`maxTokens` from the daily budget while it runs, so set `maxTokens` to what you need. Stopping
a reply early (`signal`) still counts as a full-length reply.
For structured output, ask for JSON in the prompt and `JSON.parse` the text defensively.

### Errors

Failed calls throw `eze.EzeError` with `.status` (HTTP status) and `.message`.
`429` means the user is sending too many requests; back off instead of looping.

## Not available

There are no cron jobs, server code, custom backends or secrets. Keep it to static files plus
the SDK. Do not try to call third-party APIs that need secret keys from the browser.
