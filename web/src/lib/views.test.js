import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sortFolders, groupByMonth, folderLine, needsReload } from './views.js';

const f = (name, o = {}) => ({ name, date: name.slice(0, 10).match(/^\d{4}-\d\d-\d\d$/) ? name.slice(0, 10) : '', age_days: 0, bytes: 0, files: 0, ...o });
const names = (l) => l.map((x) => x.name);

test('sortFolders: name ascending; age, size and files descending; ties by name', () => {
  const l = [f('b', { age_days: 3, bytes: 5, files: 2 }), f('a', { age_days: 3, bytes: 9, files: 2 }), f('c', { age_days: 9, bytes: 5, files: 7 })];
  assert.deepEqual(names(sortFolders(l, 'name')), ['a', 'b', 'c']);
  assert.deepEqual(names(sortFolders(l, 'age')), ['c', 'a', 'b']);
  assert.deepEqual(names(sortFolders(l, 'size')), ['a', 'b', 'c']);
  assert.deepEqual(names(sortFolders(l, 'files')), ['c', 'a', 'b']);
});

test('sortFolders does not mutate its input', () => {
  const l = [f('b'), f('a')];
  sortFolders(l, 'name');
  assert.deepEqual(names(l), ['b', 'a']);
});

test('groupByMonth groups by month, oldest first, no date last, names in order', () => {
  const g = groupByMonth([f('2025-06-01'), f('Misc'), f('2025-05-30 - Trip'), f('2025-05-02'), f('Another')]);
  assert.deepEqual(g.map((x) => [x.key, x.label]), [['2025-05', 'May 2025'], ['2025-06', 'June 2025'], ['', 'No date']]);
  assert.deepEqual(names(g[0].folders), ['2025-05-02', '2025-05-30 - Trip']);
  assert.deepEqual(names(g[2].folders), ['Another', 'Misc']);
  assert.deepEqual(groupByMonth([]), []);
});

test('folderLine says "1 file", not "1 files"', () => {
  assert.equal(folderLine({ files: 1, bytes: 2_100_000 }), '1 file · 2.1 MB');
  assert.equal(folderLine({ files: 3, bytes: 2_100_000 }), '3 files · 2.1 MB');
  assert.equal(folderLine({ files: 1234, bytes: 0 }), '1,234 files · 0 B');
});

test('needsReload only when connected, versions differ and the live one is known', () => {
  assert.equal(needsReload(3, 4, true), true);
  assert.equal(needsReload(4, 4, true), false);
  assert.equal(needsReload(3, 4, false), false);
  assert.equal(needsReload(3, 0, true), false);
});
