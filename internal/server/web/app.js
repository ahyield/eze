// eze dashboard: drag-and-drop deploys, site search and site deletion.
(() => {
  'use strict';

  const SITE_NAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
  const RESERVED = new Set(['www', 'api', 'admin', 'auth', 'login', 'static']);
  const LOCAL_ONLY = new Set(['eze.json', 'AGENTS.md', 'CLAUDE.md']); // never uploaded, like the CLI

  // slug turns a folder or file name into a site name, like the CLI does.
  function slug(s) {
    return s.toLowerCase().replace(/\.(zip|html?)$/, '').replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63).replace(/-+$/, '');
  }

  // skipped reports whether a relative path should stay out of the upload: hidden files
  // (except .well-known), node_modules, macOS clutter and the CLI's local-only files.
  function skipped(path) {
    const parts = path.split('/');
    if (parts.length === 1 && LOCAL_ONLY.has(parts[0])) return true;
    return parts.some((p) => (p.startsWith('.') && p !== '.well-known') || p === 'node_modules' || p === '__MACOSX');
  }

  function humanBytes(n) {
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
    if (n >= 1 << 10) return Math.round(n / (1 << 10)) + ' KB';
    return n + ' B';
  }

  function readEntries(dir) {
    const reader = dir.createReader();
    const all = [];
    return new Promise((resolve, reject) => {
      const next = () => reader.readEntries((batch) => {
        if (!batch.length) return resolve(all);
        all.push(...batch);
        next(); // readEntries returns at most ~100 entries per call
      }, reject);
      next();
    });
  }

  async function walk(entry, prefix, out) {
    if (entry.isFile) {
      const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
      out.push({ path: prefix + entry.name, file });
    } else if (entry.isDirectory) {
      for (const child of await readEntries(entry)) await walk(child, prefix + entry.name + '/', out);
    }
  }

  // fromFiles turns a flat file list into a deploy: one zip, or files at relative paths.
  // A single folder is unwrapped and gives the site its name.
  function fromFiles(files) {
    if (files.length === 1 && /\.zip$/i.test(files[0].path)) {
      return { name: slug(files[0].path.split('/').pop()), zip: files[0].file };
    }
    let name = '';
    const tops = new Set(files.map((f) => f.path.split('/')[0]));
    if (tops.size === 1 && files.every((f) => f.path.includes('/'))) {
      name = slug([...tops][0]);
      files = files.map((f) => ({ path: f.path.slice(f.path.indexOf('/') + 1), file: f.file }));
    }
    files = files.filter((f) => !skipped(f.path));
    // Without an index.html, the server serves a single top-level HTML page as the home
    // page (see sites.HomePage); name the site after it.
    const top = files.filter((f) => !f.path.includes('/'));
    const pages = top.filter((f) => /\.html?$/i.test(f.path) && f.path.toLowerCase() !== '404.html');
    if (pages.length === 1 && !top.some((f) => f.path === 'index.html')) name = name || slug(pages[0].path);
    return { name, files };
  }

  async function fromDrop(dataTransfer) {
    const entries = [...dataTransfer.items].map((i) => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
    const files = [];
    if (entries.length) {
      for (const e of entries) await walk(e, '', files);
    } else {
      for (const f of dataTransfer.files) files.push({ path: f.name, file: f });
    }
    return fromFiles(files);
  }

  function fromInput(input) {
    return fromFiles([...input.files].map((f) => ({ path: f.webkitRelativePath || f.name, file: f })));
  }

  // Building a .tar.gz in the browser (ustar format, which the server's Go reader accepts)

  const enc = new TextEncoder();

  function putString(buf, offset, length, s) {
    const bytes = enc.encode(s);
    if (bytes.length > length) throw new Error('field too long');
    buf.set(bytes, offset);
  }

  function putOctal(buf, offset, length, n) {
    putString(buf, offset, length, n.toString(8).padStart(length - 1, '0') + '\0');
  }

  // splitPath fits a path into ustar's 100-byte name and 155-byte prefix fields.
  function splitPath(path) {
    if (enc.encode(path).length <= 100) return ['', path];
    for (let i = path.indexOf('/'); i > 0; i = path.indexOf('/', i + 1)) {
      const prefix = path.slice(0, i);
      const name = path.slice(i + 1);
      if (enc.encode(prefix).length <= 155 && enc.encode(name).length <= 100) return [prefix, name];
    }
    throw new Error('path too long for upload: ' + path);
  }

  function tarHeader(path, size, mtime) {
    const h = new Uint8Array(512);
    const [prefix, name] = splitPath(path);
    putString(h, 0, 100, name);
    putOctal(h, 100, 8, 0o644);
    putOctal(h, 108, 8, 0);
    putOctal(h, 116, 8, 0);
    putOctal(h, 124, 12, size);
    putOctal(h, 136, 12, Math.floor(mtime / 1000));
    h.fill(0x20, 148, 156); // checksum is computed with its own field as spaces
    h[156] = 0x30; // '0': regular file
    putString(h, 257, 6, 'ustar\0');
    putString(h, 263, 2, '00');
    putString(h, 345, 155, prefix);
    let sum = 0;
    for (const b of h) sum += b;
    putString(h, 148, 8, sum.toString(8).padStart(6, '0') + '\0 ');
    return h;
  }

  async function buildTarGz(files) {
    if (typeof CompressionStream === 'undefined') throw new Error('This browser is too old to upload folders. Try a .zip instead.');
    const parts = [];
    for (const { path, file } of files) {
      parts.push(tarHeader(path, file.size, file.lastModified || Date.now()), file);
      const pad = (512 - (file.size % 512)) % 512;
      if (pad) parts.push(new Uint8Array(pad));
    }
    parts.push(new Uint8Array(1024)); // end of archive
    const gz = new Blob(parts).stream().pipeThrough(new CompressionStream('gzip'));
    return new Response(gz).blob();
  }

  function upload(name, body, contentType, onProgress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('PUT', '/_q/sites/' + encodeURIComponent(name));
      xhr.setRequestHeader('Content-Type', contentType);
      xhr.responseType = 'json';
      xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.response);
        else reject(new Error((xhr.response && xhr.response.error) || 'Deploy failed (' + xhr.status + ')'));
      };
      xhr.onerror = () => reject(new Error('Network error, the upload did not finish.'));
      xhr.send(body);
    });
  }

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') node.className = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v);
    }
    node.append(...children.filter((c) => c != null));
    return node;
  }

  function knownSites() {
    const script = document.getElementById('known-sites');
    try { return script ? JSON.parse(script.textContent) : {}; } catch { return {}; }
  }

  function setupDropzone(zone) {
    const fixedName = zone.dataset.site;
    const domain = zone.dataset.domain;
    const scheme = zone.dataset.scheme;
    const maxBytes = Number(zone.dataset.max);
    const idle = zone.querySelector('[data-idle]');
    const stage = zone.querySelector('[data-stage]');
    const known = knownSites();
    let busy = false;

    const show = (...nodes) => {
      idle.hidden = true;
      stage.hidden = false;
      stage.replaceChildren(...nodes);
    };
    const reset = () => {
      busy = false;
      stage.hidden = true;
      stage.replaceChildren();
      idle.hidden = false;
      zone.querySelectorAll('input[type=file]').forEach((i) => { i.value = ''; });
    };
    const fail = (message) => {
      busy = false;
      show(el('p', { class: 'notice warn' }, message), el('button', { class: 'btn', type: 'button', onclick: reset }, 'Try again'));
    };

    async function prepare(pick) {
      if (busy) return;
      busy = true;
      show(el('p', { class: 'summary' }, 'Reading files...'));
      let picked;
      try {
        picked = await pick();
      } catch (err) {
        return fail('Could not read what was dropped: ' + err.message);
      }
      const { zip, files } = picked;
      if (!zip && !files.length) return fail('Nothing to deploy: no files left after skipping hidden files and node_modules.');
      busy = false;
      confirm(picked);
    }

    function confirm(picked) {
      const total = picked.zip ? picked.zip.size : picked.files.reduce((n, f) => n + f.file.size, 0);
      const what = picked.zip ? picked.zip.name + ' (' + humanBytes(total) + ')' : picked.files.length + ' file' + (picked.files.length === 1 ? '' : 's') + ', ' + humanBytes(total);

      const nameInput = el('input', { type: 'text', value: fixedName || picked.name || '', 'aria-label': 'Site name', autocomplete: 'off', spellcheck: 'false' });
      const deployBtn = el('button', { class: 'btn primary', type: 'submit' }, 'Deploy');
      const status = el('p', { class: 'notice', hidden: '' });

      const check = () => {
        const name = nameInput.value.trim();
        status.hidden = true;
        status.className = 'notice';
        deployBtn.disabled = false;
        if (!SITE_NAME.test(name) || RESERVED.has(name)) {
          deployBtn.disabled = true;
          if (name) {
            status.hidden = false;
            status.className = 'notice warn';
            status.textContent = RESERVED.has(name) ? name + ' is reserved.' : 'Use lowercase letters, digits and hyphens.';
          }
          return;
        }
        const existing = fixedName ? null : known[name];
        if (existing && !existing.can_manage) {
          deployBtn.disabled = true;
          status.hidden = false;
          status.className = 'notice warn';
          status.textContent = name + ' belongs to ' + existing.owner + '. Pick another name.';
        } else if (existing) {
          status.hidden = false;
          status.className = 'notice warn';
          status.textContent = 'This replaces the existing ' + name + ', last deployed by ' + existing.deployed_by + '.';
        }
      };
      nameInput.addEventListener('input', check);

      const form = el('form', {
        onsubmit: (e) => { e.preventDefault(); if (!deployBtn.disabled) deploy(nameInput.value.trim(), picked); },
      },
        el('p', { class: 'summary' }, 'Ready to deploy ' + what + '.'),
        fixedName ? null : el('div', { class: 'row' }, el('label', {}, 'Site name'), nameInput, el('span', { class: 'suffix mono' }, '.' + domain)),
        status,
        el('div', { class: 'row' }, deployBtn, el('button', { class: 'btn', type: 'button', onclick: reset }, 'Cancel')),
      );
      show(form);
      check();
      if (!fixedName) nameInput.focus();
    }

    async function deploy(name, picked) {
      if (busy) return;
      busy = true;
      const bar = el('div');
      const label = el('p', { class: 'summary' }, picked.zip ? 'Uploading...' : 'Packing files...');
      show(label, el('div', { class: 'bar' }, bar));
      try {
        const body = picked.zip || await buildTarGz(picked.files);
        if (body.size > maxBytes) {
          throw new Error('That is ' + humanBytes(body.size) + ' compressed; the limit is ' + humanBytes(maxBytes) + '.');
        }
        label.textContent = 'Uploading ' + humanBytes(body.size) + '...';
        const res = await upload(name, body, picked.zip ? 'application/zip' : 'application/gzip', (p) => {
          bar.style.width = Math.round(p * 100) + '%';
          if (p >= 1) label.textContent = 'Publishing...';
        });
        busy = false;
        const url = res.url || scheme + '://' + name + '.' + domain + '/';
        show(
          el('p', { class: 'success' }, name + ' is live.'),
          el('p', { class: 'summary' }, el('a', { href: url }, url)),
          el('div', { class: 'row' },
            el('a', { class: 'btn primary', href: url }, 'Open site'),
            fixedName ? el('button', { class: 'btn', type: 'button', onclick: () => location.reload() }, 'Refresh') : el('a', { class: 'btn', href: '/sites/' + name }, 'Site details'),
          ),
        );
      } catch (err) {
        fail(err.message);
      }
    }

    let depth = 0; // dragenter/leave fire for children too
    zone.addEventListener('dragenter', (e) => { e.preventDefault(); depth++; zone.classList.add('over'); });
    zone.addEventListener('dragover', (e) => { e.preventDefault(); e.dataTransfer.dropEffect = 'copy'; });
    zone.addEventListener('dragleave', () => { if (--depth <= 0) { depth = 0; zone.classList.remove('over'); } });
    zone.addEventListener('drop', (e) => {
      e.preventDefault();
      depth = 0;
      zone.classList.remove('over');
      prepare(() => fromDrop(e.dataTransfer));
    });
    zone.querySelectorAll('[data-pick-folder], [data-pick-file]').forEach((input) => {
      input.addEventListener('change', () => { if (input.files.length) prepare(async () => fromInput(input)); });
    });
    // A file dropped outside the zone would navigate away from the page; ignore it instead.
    window.addEventListener('dragover', (e) => e.preventDefault());
    window.addEventListener('drop', (e) => e.preventDefault());
  }

  function setupSearch(input) {
    const rows = [...document.querySelectorAll('[data-site-table] tbody tr')];
    const none = document.querySelector('[data-no-results]');
    input.addEventListener('input', () => {
      const q = input.value.trim().toLowerCase();
      let shown = 0;
      for (const row of rows) {
        const match = !q || row.dataset.search.toLowerCase().includes(q);
        row.hidden = !match;
        if (match) shown++;
      }
      if (none) none.hidden = shown > 0;
    });
  }

  function setupDelete(form) {
    const name = form.dataset.deleteSite;
    const input = form.querySelector('[data-confirm]');
    const button = form.querySelector('button');
    const error = form.querySelector('[data-error]');
    input.addEventListener('input', () => { button.disabled = input.value.trim() !== name; });
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      if (input.value.trim() !== name) return;
      button.disabled = true;
      error.hidden = true;
      try {
        const res = await fetch('/_q/sites/' + encodeURIComponent(name), { method: 'DELETE', credentials: 'same-origin' });
        if (!res.ok) {
          const data = await res.json().catch(() => null);
          throw new Error((data && data.error) || 'Delete failed (' + res.status + ')');
        }
        location.href = '/';
      } catch (err) {
        error.textContent = err.message;
        error.hidden = false;
        button.disabled = false;
      }
    });
  }

  if (typeof document !== 'undefined') {
    document.querySelectorAll('[data-drop]').forEach(setupDropzone);
    document.querySelectorAll('[data-site-search]').forEach(setupSearch);
    document.querySelectorAll('[data-delete-site]').forEach(setupDelete);
  }

  // Exposed for tests.
  if (typeof window !== 'undefined') window.ezeApp = { slug, skipped, fromFiles, buildTarGz, tarHeader };
})();
