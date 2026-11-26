// Pure helpers for the staging list and timeline views: no DOM, no Svelte.
import { formatBytes, formatCount } from './format.js';

const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
const byName = (a, b) => cmp(a.name, b.name);
const BY = {
  name: byName,
  age: (a, b) => b.age_days - a.age_days || byName(a, b),
  size: (a, b) => b.bytes - a.bytes || byName(a, b),
  files: (a, b) => b.files - a.files || byName(a, b),
};

// sortFolders returns a sorted copy: name ascending; age (oldest first),
// size and files descending; ties by name.
export function sortFolders(folders, by) {
  return [...folders].sort(BY[by] ?? byName);
}

const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];

// groupByMonth groups folders by the month of their date (oldest month
// first, folders in name order); folders without a date come last.
export function groupByMonth(folders) {
  const groups = new Map();
  for (const f of [...folders].sort(byName)) {
    const key = f.date ? f.date.slice(0, 7) : '';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(f);
  }
  const label = (key) => (key ? `${MONTHS[Number(key.slice(5, 7)) - 1] ?? key.slice(5, 7)} ${key.slice(0, 4)}` : 'No date');
  return [...groups.keys()]
    .sort((a, b) => (a === '' ? 1 : b === '' ? -1 : cmp(a, b)))
    .map((key) => ({ key, label: label(key), folders: groups.get(key) }));
}

// folderLine: '1 file · 2.1 MB', '3 files · 2.1 MB'.
export const folderLine = (f) => `${formatCount(f.files)} ${f.files === 1 ? 'file' : 'files'} · ${formatBytes(f.bytes)}`;

// needsReload: the data on screen is not the version the stream says is
// current (F10: a load can finish just before its own event arrives).
export const needsReload = (dataVersion, liveVersion, connected) => !!connected && liveVersion !== 0 && dataVersion !== liveVersion;
