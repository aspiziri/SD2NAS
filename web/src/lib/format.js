// Pure formatting helpers for the UI: no DOM and no Svelte, tested with node --test.

const UNITS = ['KB', 'MB', 'GB', 'TB'];
const pad = (n) => String(n).padStart(2, '0');

// formatBytes(1234567) === '1.2 MB' (decimal units, like the ingest messages).
export function formatBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '0 B';
  // Pick the unit after rounding, so 999.6 is "1.0 KB" and 999999 is "1.0 MB".
  if (Math.round(n) < 1000) return `${Math.round(n)} B`;
  let v = n / 1000;
  let i = 0;
  while (Math.round(v * 10) / 10 >= 1000 && i < UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(1)} ${UNITS[i]}`;
}

export const formatCount = (n) => Number(n ?? 0).toLocaleString('en-US');

// formatWhen renders an RFC 3339 instant in the browser's zone: "2026-11-21 19:42".
export function formatWhen(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// formatCapture shows a capture wall clock ("2025-05-31T01:30:12") as named, with no zone conversion.
export const formatCapture = (s) => (s ? s.replace('T', ' ') : '');

export function dateRange(dates) {
  const d = [...(dates ?? [])].sort();
  if (d.length === 0) return '';
  return d.length === 1 ? d[0] : `${d[0]} → ${d[d.length - 1]}`;
}

const PHASES = [
  ['scanning', 'Reading the card'],
  ['classifying', 'Checking against the catalog'],
  ['copying', 'Copying new files'],
  ['finishing', 'Finishing'],
];

// phaseSteps lists the import phases with state 'done' | 'current' | 'todo'.
export function phaseSteps(phase) {
  const at = phase === 'done' ? PHASES.length : PHASES.findIndex(([k]) => k === phase);
  return PHASES.map(([key, label], i) => ({
    key,
    label,
    state: at < 0 ? 'todo' : i < at ? 'done' : i === at ? 'current' : 'todo',
  }));
}

const frac = (done, total) => (total > 0 ? Math.min(1, Math.max(0, done / total)) : 0);

// progressFraction is the progress bar value for a ProgressSnapshot (0–1).
export function progressFraction(p) {
  if (!p) return 0;
  switch (p.phase) {
    case 'classifying':
      return frac(p.files_done, p.files_total);
    case 'copying':
      return p.bytes_total > 0 ? frac(p.bytes_done, p.bytes_total) : frac(p.files_done, p.files_total);
    case 'finishing':
    case 'done':
      return 1;
    default:
      return 0;
  }
}

const STATUS = { queued: 'Waiting', running: 'Importing', done: 'Done', failed: 'Failed', refused: 'Refused', cancelled: 'Cancelled' };
export const statusLabel = (s) => STATUS[s] ?? s;
export const levelClass = (level) => (level === 'alert' ? 'bad' : level === 'warning' ? 'warn' : 'ok');
export const statusClass = (i) => (i.status === 'queued' || i.status === 'running' ? 'live' : levelClass(i.level));

export function cardName(i) {
  if (i.card_label) return i.card_label;
  const parts = (i.card_path ?? '').split(/[\\/]/).filter(Boolean);
  return parts.at(-1) ?? 'card';
}

export const knownCount = (c) => (c.in_archive ?? 0) + (c.in_staging ?? 0) + (c.seen_before ?? 0);

// sessionFolder: 'Archive/2026-11-08 - Big Sur/Capture/x.ARW' → 'Archive/2026-11-08 - Big Sur'.
export function sessionFolder(relPath) {
  if (!relPath) return '';
  const parts = relPath.split('/');
  return parts.length >= 3 ? parts.slice(0, 2).join('/') : parts.slice(0, -1).join('/');
}

// groupKnown counts the already-known files (in_archive, in_staging) by the
// session folder they live in, biggest first, then by name.
export function groupKnown(files) {
  const counts = new Map();
  for (const f of files) {
    if (f.outcome !== 'in_archive' && f.outcome !== 'in_staging') continue;
    const folder = sessionFolder(f.rel_path) || '(location unknown)';
    counts.set(folder, (counts.get(folder) ?? 0) + 1);
  }
  return [...counts]
    .map(([folder, count]) => ({ folder, count }))
    .sort((a, b) => b.count - a.count || (a.folder < b.folder ? -1 : a.folder > b.folder ? 1 : 0));
}

export const outcomeFiles = (files, outcome) => files.filter((f) => f.outcome === outcome);

// kindChips are the folder view's filter chips: All, then each kind present.
export function kindChips(c) {
  const chips = [{ kind: '', label: 'All', count: c.jpg + c.raw + c.video }];
  for (const [kind, label] of [['raw', 'RAW'], ['jpg', 'JPG'], ['video', 'Video']]) {
    if (c[kind]) chips.push({ kind, label, count: c[kind] });
  }
  return chips;
}

export function extLabel(file) {
  const dot = file.name.lastIndexOf('.');
  return dot < 0 ? '' : file.name.slice(dot + 1).toUpperCase();
}
