// eze client SDK. Include with <script src="/_q/sdk.js"></script> and use window.eze.
// Every call is same-origin and authenticated by your existing sign-in, so there are no keys to manage.
(() => {
  'use strict';

  class EzeError extends Error {
    constructor(status, message) {
      super(message);
      this.name = 'EzeError';
      this.status = status;
    }
  }

  async function request(method, path, body) {
    const init = { method, headers: {}, credentials: 'same-origin' };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetch('/_q' + path, init);
    if (res.status === 204) return null;
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new EzeError(res.status, (data && data.error) || res.statusText);
    return data;
  }

  const enc = encodeURIComponent;

  class Collection {
    constructor(name) {
      this.name = name;
      this.path = '/db/' + enc(name);
    }

    // list({ where: { status: 'draft' }, limit: 100, offset: 0, order: 'asc' | 'desc' })
    list(opts = {}) {
      const q = new URLSearchParams();
      if (opts.where) q.set('where', JSON.stringify(opts.where));
      if (opts.limit != null) q.set('limit', String(opts.limit));
      if (opts.offset != null) q.set('offset', String(opts.offset));
      if (opts.order) q.set('order', opts.order);
      const qs = q.toString();
      return request('GET', this.path + (qs ? '?' + qs : ''));
    }

    // find(where) is list({ where }).
    find(where, opts = {}) {
      return this.list({ ...opts, where });
    }

    async get(id) {
      try {
        return await request('GET', this.path + '/' + enc(id));
      } catch (err) {
        if (err.status === 404) return null;
        throw err;
      }
    }

    create(data) {
      return request('POST', this.path, data);
    }

    // set creates or replaces the document with this id.
    set(id, data) {
      return request('PUT', this.path + '/' + enc(id), data);
    }

    // update shallow-merges fields into an existing document; a null value removes the field.
    update(id, patch) {
      return request('PATCH', this.path + '/' + enc(id), patch);
    }

    delete(id) {
      return request('DELETE', this.path + '/' + enc(id));
    }

    // subscribe({ onCreate(doc), onUpdate(doc), onDelete(id), onReconnect() }) returns an unsubscribe function.
    // onReconnect fires after a dropped connection comes back; refetch there, since events may have been missed.
    subscribe(handlers = {}) {
      const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/_q/subscribe/' + enc(this.name);
      let ws;
      let closed = false;
      let attempt = 0;
      let timer;

      const connect = () => {
        ws = new WebSocket(url);
        ws.onopen = () => {
          if (attempt > 0 && handlers.onReconnect) handlers.onReconnect();
          attempt = 0;
        };
        ws.onmessage = (msg) => {
          let ev;
          try { ev = JSON.parse(msg.data); } catch { return; }
          if (ev.type === 'create' && handlers.onCreate) handlers.onCreate(ev.doc);
          else if (ev.type === 'update' && handlers.onUpdate) handlers.onUpdate(ev.doc);
          else if (ev.type === 'delete' && handlers.onDelete) handlers.onDelete(ev.id);
        };
        ws.onclose = () => {
          if (closed) return;
          attempt++;
          const delay = Math.min(30000, 500 * 2 ** Math.min(attempt, 6)) * (0.5 + Math.random() / 2);
          timer = setTimeout(connect, delay);
        };
      };
      connect();

      return () => {
        closed = true;
        clearTimeout(timer);
        if (ws) ws.close();
      };
    }
  }

  const collections = new Map();
  const db = {
    collection(name) {
      if (!collections.has(name)) collections.set(name, new Collection(name));
      return collections.get(name);
    },
  };

  let mePromise;
  const id = {
    // me() resolves to { email, name } for whoever is viewing the page.
    me() {
      if (!mePromise) mePromise = request('GET', '/me').catch((err) => { mePromise = undefined; throw err; });
      return mePromise;
    },
  };

  const files = {
    // upload(file, { name, onProgress(fraction) }) resolves to { id, name, size, content_type, url, ... }.
    // file is a File or Blob, e.g. from <input type="file"> or a canvas.
    upload(file, opts = {}) {
      const form = new FormData();
      form.append('file', file, opts.name || file.name || 'file');
      // XHR rather than fetch, because fetch can't report upload progress.
      return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', '/_q/files');
        xhr.responseType = 'json';
        if (opts.onProgress) {
          xhr.upload.onprogress = (e) => { if (e.lengthComputable) opts.onProgress(e.loaded / e.total); };
        }
        xhr.onload = () => {
          if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.response);
          else reject(new EzeError(xhr.status, (xhr.response && xhr.response.error) || xhr.statusText));
        };
        xhr.onerror = () => reject(new EzeError(0, 'network error'));
        xhr.send(form);
      });
    },

    // list({ limit, offset }) returns the site's files, newest first.
    list(opts = {}) {
      const q = new URLSearchParams();
      if (opts.limit != null) q.set('limit', String(opts.limit));
      if (opts.offset != null) q.set('offset', String(opts.offset));
      const qs = q.toString();
      return request('GET', '/files' + (qs ? '?' + qs : ''));
    },

    async get(id) {
      try {
        return await request('GET', '/files/' + enc(id));
      } catch (err) {
        if (err.status === 404) return null;
        throw err;
      }
    },

    delete(id) {
      return request('DELETE', '/files/' + enc(id));
    },
  };

  window.eze = {
    site: location.hostname.split('.')[0],
    db,
    id,
    files,
    EzeError,
  };
})();
