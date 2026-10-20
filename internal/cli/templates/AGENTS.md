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

Use this for live updates across everyone viewing the site (polls, boards, chat, games).
Load the initial state with `list()`, then apply events.

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

### Errors

Failed calls throw `eze.EzeError` with `.status` (HTTP status) and `.message`.
`429` means the user is sending too many requests; back off instead of looping.

## Not available

There are no cron jobs, server code, custom backends or secrets. Keep it to static files plus
the SDK. Do not try to call third-party APIs that need secret keys from the browser.
