// Thin fetch wrapper shared by every tab. Same-origin only: the dashboard
// listener serves both the page and /api/*.
export const api = {
  // GET a JSON endpoint. Throws an Error whose message is the server's
  // {"error": ...} when there is one, else the status line.
  async get(path, init = {}) {
    const r = await fetch(path, { cache: "no-store", ...init });
    if (!r.ok) {
      let msg = `${r.status} ${r.statusText}`;
      try {
        const body = await r.json();
        if (body && body.error) msg = body.error;
      } catch (_) {
        /* not JSON */
      }
      throw new Error(msg);
    }
    return r.json();
  },

  // Server-sent events. handlers is {snapshot(data), request(data), open(),
  // error(e)} keyed by event name; unknown events are ignored. Returns a
  // cancel function. Reconnects with backoff on failure. The /api/events
  // endpoint is its own leaf; this client is complete already.
  sse(path, handlers = {}) {
    let es = null;
    let closed = false;
    let backoff = 1000;
    const open = () => {
      if (closed) return;
      es = new EventSource(path);
      es.onopen = () => {
        backoff = 1000;
        handlers.open?.();
      };
      for (const name of Object.keys(handlers)) {
        if (name === "open" || name === "error") continue;
        es.addEventListener(name, (ev) => {
          let data = null;
          try {
            data = JSON.parse(ev.data);
          } catch (_) {
            return;
          }
          handlers[name](data);
        });
      }
      es.onerror = (e) => {
        handlers.error?.(e);
        es.close();
        if (!closed) setTimeout(open, backoff);
        backoff = Math.min(backoff * 2, 30000);
      };
    };
    open();
    return () => {
      closed = true;
      if (es) es.close();
    };
  },
};
