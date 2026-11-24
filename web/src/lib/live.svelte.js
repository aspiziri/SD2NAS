// Live server state pushed over /api/events (server-sent events).
// Note: when the catalog can't open, the server mounts no ingest routes and
// never sends ingest.current. "Ingest unavailable" is learned from /healthz
// (503 + problems) by App.svelte's HealthBanner, not from this store.
export const live = $state({
  connected: false,
  current: { running: null, queued: [] }, // the last ingest.current
  finished: null, // the last ingest.finished
  finishedCount: 0, // bumps on every ingest.finished; pages reload on it
  staging: { version: 0, rescanning: false, last_rescan: null }, // the last staging.changed
  promote: null, // the last promote.progress: { running, queued: [...], last }; null until it arrives
  indexer: { version: 0, running: null, last: null, last_backup: null, backing_up: false, pending_full: false, changed: 0, missing: 0, next_scan: null }, // the last indexer.progress
});

let source = null;
let retry = 0;

export function connectEvents() {
  if (source) return;
  source = new EventSource('/api/events');
  source.onopen = () => {
    live.connected = true;
  };
  source.onerror = () => {
    live.connected = false;
    // While readyState is CONNECTING, EventSource retries by itself. A
    // CLOSED stream (an HTTP error or wrong content type) is reopened here.
    if (source.readyState === EventSource.CLOSED) {
      source = null;
      clearTimeout(retry);
      retry = setTimeout(connectEvents, 5000);
    }
  };
  source.addEventListener('ingest.current', (e) => {
    live.current = JSON.parse(e.data);
  });
  source.addEventListener('ingest.finished', (e) => {
    live.finished = JSON.parse(e.data);
    live.finishedCount += 1;
  });
  source.addEventListener('staging.changed', (e) => {
    live.staging = JSON.parse(e.data);
  });
  source.addEventListener('promote.progress', (e) => {
    live.promote = JSON.parse(e.data);
  });
  source.addEventListener('indexer.progress', (e) => {
    live.indexer = JSON.parse(e.data);
  });
}

// Phones kill background connections; reconnect when the app is shown again.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && !source) connectEvents();
});
