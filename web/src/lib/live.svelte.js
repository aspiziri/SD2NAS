// Live server state pushed over /api/events (server-sent events).
// Note: when the catalog can't open, the server mounts no ingest routes and
// never sends ingest.current. "Ingest unavailable" is learned from /healthz
// (503 + problems) by App.svelte's HealthBanner, not from this store.
export const live = $state({
  connected: false,
  current: { running: null, queued: [] }, // the last ingest.current
  finished: null, // the last ingest.finished
  finishedCount: 0, // bumps on every ingest.finished; pages reload on it
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
}

// Phones kill background connections; reconnect when the app is shown again.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && !source) connectEvents();
});
