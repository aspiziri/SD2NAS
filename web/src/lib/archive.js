// Pure helpers for the Archive index page: no DOM and no Svelte, tested with node --test.
// `p` is the indexer's Progress (Status.running), `r` a ScanResult, `b` a BackupResult.
import { formatBytes, formatCount, formatWhen } from './format.js';
import { plural, joinList } from './messages.js';

// scanPercent is the progress bar value: null while there is nothing to measure
// (starting, listing), 0-99 while hashing, 100 while checking for gone files.
export function scanPercent(p) {
  if (!p || p.phase === '' || p.phase === 'listing') return null;
  if (p.phase === 'checking') return 100;
  let pct = 0;
  if (p.bytes_to_hash > 0) pct = Math.floor((100 * p.bytes_hashed) / p.bytes_to_hash);
  else if (p.to_hash > 0) pct = Math.floor((100 * p.hashed) / p.to_hash);
  return Math.max(0, Math.min(99, pct));
}

// scanLine is the sentence under the progress bar. A pause overrides the phase text.
export function scanLine(p) {
  if (!p) return '';
  if (p.paused_by === 'user') return 'Paused. Press Resume to carry on.';
  if (p.paused_by === 'busy') return 'Paused while an import or promote runs. It carries on by itself.';
  const verb = p.full ? 'Re-indexing' : 'Rescanning';
  switch (p.phase) {
    case '':
      return `${verb}… starting`;
    case 'listing':
      return `${verb}… looking for files (${formatCount(p.listed)} found so far)`;
    case 'hashing':
      return p.full
        ? `${verb}… ${scanPercent(p)}% (every file is read again)`
        : `${verb}… ${scanPercent(p)}% (only files whose size or modified time changed get re-hashed)`;
    case 'checking':
      return `${verb}… checking for files that are gone`;
    default:
      return '';
  }
}

// progressDetail is the files and bytes done of the total, under the sentence.
export function progressDetail(p) {
  if (!p) return '';
  if (p.phase === 'hashing') {
    return `${formatCount(p.hashed)} of ${formatCount(p.to_hash)} files · ${formatBytes(p.bytes_hashed)} of ${formatBytes(p.bytes_to_hash)}`;
  }
  if (p.phase === 'listing') return `${formatCount(p.listed)} files found`;
  return '';
}

// scanSummary is the toast after a manual rescan or re-index finishes.
export function scanSummary(r) {
  const noun = r.full ? 'Re-index' : 'Rescan';
  if (r.skipped) return `${noun} skipped: ${r.skipped}`;
  if (r.stopped) return `${noun} stopped before it finished. It carries on at the next scan.`;
  const parts = [];
  if (r.added) parts.push(`${formatCount(r.added)} new`);
  if (r.moved) parts.push(`${formatCount(r.moved)} moved`);
  if (r.changed) parts.push(`${formatCount(r.changed)} changed`);
  if (r.reappeared) parts.push(`${formatCount(r.reappeared)} back`);
  if (r.recovered) parts.push(`${formatCount(r.recovered)} recovered`);
  if (r.missing) parts.push(`${formatCount(r.missing)} missing`);
  if (r.copies) parts.push(plural(r.copies, 'duplicate', 'duplicates'));
  let s = `${noun} complete: ${parts.length ? joinList(parts) : 'no changes'}.`;
  if (r.busy_folders) s += ` ${plural(r.busy_folders, 'folder was', 'folders were')} being promoted and will be scanned next time.`;
  if (r.errors?.length) s += ` ${plural(r.errors.length, 'problem', 'problems')}: ${r.errors[0]}`;
  return s;
}

// changedHeading heads the "changed since indexed" card.
export const changedHeading = (n) =>
  `${plural(n, 'archive file', 'archive files')} changed since ${n === 1 ? 'it was' : 'they were'} indexed`;

// sizeChange: 'same size', '+12.3 KB' or '-12.3 KB'.
export function sizeChange(oldSize, size) {
  if (oldSize === size) return 'same size';
  return `${size > oldSize ? '+' : '-'}${formatBytes(Math.abs(size - oldSize))}`;
}

export const lastScanLabel = (last) => (last ? formatWhen(last.finished_at) : 'Never');

export function backupLine(b) {
  if (!b) return 'No catalog backup yet.';
  if (b.error) return `The last catalog backup failed: ${b.error}`;
  return `Last catalog backup ${formatWhen(b.at)}, ${formatBytes(b.bytes)}.`;
}
