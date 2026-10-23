// eze client SDK. Include with <script src="/_eze/sdk.js"></script> and use window.eze.
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
    const res = await fetch('/_eze' + path, init);
    if (res.status === 204) return null;
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new EzeError(res.status, (data && data.error) || res.statusText);
    return data;
  }

  const enc = encodeURIComponent;

  function wsURL(path) {
    return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/_eze' + path;
  }

  // backoff returns the reconnect delay for attempt n: exponential, capped at 30s, with jitter.
  function backoff(n) {
    return Math.min(30000, 500 * 2 ** Math.min(n, 6)) * (0.5 + Math.random() / 2);
  }

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
      const url = wsURL('/subscribe/' + enc(this.name));
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
          timer = setTimeout(connect, backoff(attempt));
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

  // Room is a live channel between everyone on the page: messages plus presence.
  // Peers are { id, user: { email, name }, state }. Each browser tab is its own peer.
  class Room {
    constructor(name, opts = {}) {
      this.name = name;
      this.id = null;           // this tab's peer id, set once connected
      this.peers = new Map();   // the other peers, by id
      this.state = opts.state === undefined ? null : opts.state;
      this._handlers = {};
      this._queue = [];
      this._closed = false;
      this._attempt = 0;
      this._stateTimer = null;
      this._stateDirty = false;
      this._connect();
    }

    // on('ready' | 'join' | 'leave' | 'presence' | 'message' | 'disconnect' | 'error', fn) returns an off function.
    //   ready(peers)          connected (again); peers is the full list, so re-render from it
    //   join(peer) / leave(peer) / presence(peer)
    //   message(data, peer)
    on(event, fn) {
      (this._handlers[event] = this._handlers[event] || []).push(fn);
      return () => this.off(event, fn);
    }

    off(event, fn) {
      this._handlers[event] = (this._handlers[event] || []).filter((h) => h !== fn);
    }

    // send(data) delivers data to every other peer.
    send(data) {
      this._send({ type: 'message', data });
    }

    // sendTo(peerId, data) delivers data to one peer, e.g. for WebRTC signalling.
    sendTo(peerId, data) {
      this._send({ type: 'message', to: peerId, data });
    }

    // setState(state) replaces this tab's presence state (cursor, selection, status...).
    // Calls are coalesced to at most 20 per second, always ending with the latest state.
    setState(state) {
      this.state = state;
      if (this._stateTimer) {
        this._stateDirty = true;
        return;
      }
      this._send({ type: 'presence', state });
      this._stateTimer = setTimeout(() => {
        this._stateTimer = null;
        if (this._stateDirty) {
          this._stateDirty = false;
          this.setState(this.state);
        }
      }, 50);
    }

    leave() {
      this._closed = true;
      clearTimeout(this._timer);
      clearTimeout(this._stateTimer);
      if (this._ws) this._ws.close();
    }

    _emit(event, ...args) {
      for (const fn of this._handlers[event] || []) {
        try { fn(...args); } catch (err) { console.error(err); }
      }
    }

    // _send queues messages until connected, keeping at most the last 100.
    _send(msg) {
      if (this._ws && this._ws.readyState === WebSocket.OPEN && this.id) {
        this._ws.send(JSON.stringify(msg));
        return;
      }
      if (msg.type === 'presence') {
        return; // the current state is sent on connect anyway
      }
      this._queue.push(msg);
      if (this._queue.length > 100) this._queue.shift();
    }

    _connect() {
      const ws = new WebSocket(wsURL('/ws/' + enc(this.name)));
      this._ws = ws;
      ws.onmessage = (msg) => {
        let ev;
        try { ev = JSON.parse(msg.data); } catch { return; }
        switch (ev.type) {
          case 'welcome': {
            this.id = ev.id;
            this._attempt = 0;
            this.peers = new Map(ev.peers.map((p) => [p.id, p]));
            if (this.state !== null) ws.send(JSON.stringify({ type: 'presence', state: this.state }));
            const queued = this._queue;
            this._queue = [];
            queued.forEach((m) => ws.send(JSON.stringify(m)));
            this._emit('ready', [...this.peers.values()]);
            break;
          }
          case 'join':
            this.peers.set(ev.peer.id, ev.peer);
            this._emit('join', ev.peer);
            break;
          case 'leave': {
            const peer = this.peers.get(ev.id) || { id: ev.id, user: null, state: null };
            this.peers.delete(ev.id);
            this._emit('leave', peer);
            break;
          }
          case 'presence': {
            const peer = this.peers.get(ev.id);
            if (peer) {
              peer.state = ev.state;
              this._emit('presence', peer);
            }
            break;
          }
          case 'message':
            this._emit('message', ev.data, this.peers.get(ev.from) || { id: ev.from, user: null, state: null });
            break;
          case 'error':
            this._emit('error', new EzeError(0, ev.error));
            break;
        }
      };
      ws.onclose = () => {
        const wasConnected = this.id !== null;
        this.id = null;
        if (wasConnected) this._emit('disconnect');
        if (this._closed) return;
        this._attempt++;
        this._timer = setTimeout(() => this._connect(), backoff(this._attempt));
      };
    }
  }

  const ws = {
    // join(name, { state }) connects to a room on this site and returns a Room.
    join(name, opts) {
      return new Room(name, opts);
    },
  };

  // readEvents parses a server-sent event stream, calling onEvent(name, data) for each event.
  async function readEvents(body, onEvent) {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let end;
      while ((end = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, end);
        buf = buf.slice(end + 2);
        let name = 'message';
        let data = '';
        for (const line of chunk.split('\n')) {
          if (line.startsWith('event: ')) name = line.slice(7);
          else if (line.startsWith('data: ')) data += line.slice(6);
        }
        onEvent(name, data ? JSON.parse(data) : null);
      }
    }
  }

  const ai = {
    // chat(prompt | messages, opts) resolves to { text, model, stop_reason, usage }.
    //   prompt:   a string, or messages: [{ role: 'user' | 'assistant', content: string }, ...]
    //   opts:     { system, model, maxTokens, effort, onText(chunk, textSoFar), signal }
    // With onText the reply streams in as it's written. stop_reason 'refusal' means the
    // model declined; 'max_tokens' means the reply was cut off.
    async chat(input, opts = {}) {
      const messages = typeof input === 'string' ? [{ role: 'user', content: input }] : input;
      const body = {
        messages,
        system: opts.system,
        model: opts.model,
        max_tokens: opts.maxTokens,
        effort: opts.effort,
        stream: Boolean(opts.onText),
      };
      const res = await fetch('/_eze/ai/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify(body),
        signal: opts.signal,
      });
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        throw new EzeError(res.status, (data && data.error) || res.statusText);
      }
      if (!opts.onText) return res.json();

      let text = '';
      let result = null;
      let failure = null;
      await readEvents(res.body, (name, data) => {
        if (name === 'text') {
          text += data.text;
          opts.onText(data.text, text);
        } else if (name === 'done') {
          result = data;
        } else if (name === 'error') {
          failure = new EzeError(data.status, data.error);
        }
      });
      if (failure) throw failure;
      if (!result) throw new EzeError(0, 'the reply was interrupted');
      return result;
    },

    // usage() resolves to { enabled, used, limit, resets_at, models, default_model, max_tokens_cap }
    // for the signed-in user (budgets are per person, across all sites).
    usage() {
      return request('GET', '/ai/usage');
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
        xhr.open('POST', '/_eze/files');
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
    ws,
    ai,
    EzeError,
  };
})();
