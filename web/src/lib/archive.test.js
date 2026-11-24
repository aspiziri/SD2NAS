import { test } from 'node:test';
import assert from 'node:assert/strict';
import { scanPercent, scanLine, progressDetail, scanSummary, sizeChange, lastScanLabel, backupLine } from './archive.js';
import { formatWhen } from './format.js';

const prog = (o) => ({ trigger: 'manual', full: false, phase: 'hashing', listed: 0, to_hash: 0, hashed: 0,
  bytes_to_hash: 0, bytes_hashed: 0, folder: '', current: '', paused_by: '', ...o });
const result = (o) => ({ full: false, added: 0, moved: 0, changed: 0, reappeared: 0, recovered: 0, missing: 0, copies: 0,
  busy_folders: 0, errors: [], skipped: '', stopped: false, ...o });

test('scanPercent is null until there is something to measure', () => {
  assert.equal(scanPercent(null), null);
  assert.equal(scanPercent(prog({ phase: '' })), null);
  assert.equal(scanPercent(prog({ phase: 'listing' })), null);
});

test('scanPercent counts bytes while hashing and never reaches 100 before checking', () => {
  assert.equal(scanPercent(prog({ to_hash: 10, hashed: 5, bytes_to_hash: 1000, bytes_hashed: 250 })), 25);
  assert.equal(scanPercent(prog({ to_hash: 10, hashed: 10, bytes_to_hash: 1000, bytes_hashed: 1000 })), 99);
  assert.equal(scanPercent(prog({ to_hash: 10, hashed: 9, bytes_to_hash: 1000, bytes_hashed: 999 })), 99);
});

test('scanPercent falls back to the file count, then to 0', () => {
  assert.equal(scanPercent(prog({ to_hash: 4, hashed: 1 })), 25);
  assert.equal(scanPercent(prog({ to_hash: 4, hashed: 4 })), 99);
  assert.equal(scanPercent(prog({})), 0);
});

test('scanPercent counts only the files to hash, not the ones listed', () => {
  assert.equal(scanPercent(prog({ listed: 50000, to_hash: 2, hashed: 1 })), 50);
});

test('scanPercent is 100 while checking', () => {
  assert.equal(scanPercent(prog({ phase: 'checking', to_hash: 3, hashed: 3 })), 100);
});

test('scanLine for a rescan', () => {
  assert.equal(scanLine(null), '');
  assert.equal(scanLine(prog({ phase: '' })), 'Rescanning… starting');
  assert.equal(scanLine(prog({ phase: 'listing', listed: 1234 })), 'Rescanning… looking for files (1,234 found so far)');
  assert.equal(scanLine(prog({ to_hash: 10, hashed: 5, bytes_to_hash: 1000, bytes_hashed: 500 })),
    'Rescanning… 50% (only files whose size or modified time changed get re-hashed)');
  assert.equal(scanLine(prog({ phase: 'checking' })), 'Rescanning… checking for files that are gone');
});

test('scanLine for a re-index', () => {
  const f = { full: true };
  assert.equal(scanLine(prog({ ...f, phase: '' })), 'Re-indexing… starting');
  assert.equal(scanLine(prog({ ...f, phase: 'listing', listed: 7 })), 'Re-indexing… looking for files (7 found so far)');
  assert.equal(scanLine(prog({ ...f, to_hash: 10, hashed: 5, bytes_to_hash: 1000, bytes_hashed: 500 })),
    'Re-indexing… 50% (every file is read again)');
  assert.equal(scanLine(prog({ ...f, phase: 'checking' })), 'Re-indexing… checking for files that are gone');
});

test('scanLine says who paused the pass, whatever the phase and kind of pass', () => {
  for (const full of [false, true]) {
    for (const phase of ['listing', 'hashing', 'checking']) {
      assert.equal(scanLine(prog({ full, phase, paused_by: 'user' })), 'Paused. Press Resume to carry on.');
      assert.equal(scanLine(prog({ full, phase, paused_by: 'busy' })), 'Paused while an import or promote runs. It carries on by itself.');
    }
  }
});

test('progressDetail', () => {
  assert.equal(progressDetail(prog({ to_hash: 2000, hashed: 1234, bytes_to_hash: 5_000_000, bytes_hashed: 1_234_567 })),
    '1,234 of 2,000 files · 1.2 MB of 5.0 MB');
  assert.equal(progressDetail(prog({ phase: 'listing', listed: 1500 })), '1,500 files found');
  assert.equal(progressDetail(prog({ phase: 'checking' })), '');
  assert.equal(progressDetail(prog({ phase: '' })), '');
  assert.equal(progressDetail(null), '');
});

test('scanSummary lists the parts in order', () => {
  assert.equal(scanSummary(result({})), 'Rescan complete: no changes.');
  assert.equal(scanSummary(result({ added: 3 })), 'Rescan complete: 3 new.');
  assert.equal(scanSummary(result({ added: 3, moved: 2 })), 'Rescan complete: 3 new and 2 moved.');
  assert.equal(scanSummary(result({ copies: 1, missing: 2, recovered: 4, reappeared: 5, changed: 6, moved: 7, added: 1234 })),
    'Rescan complete: 1,234 new, 7 moved, 6 changed, 5 back, 4 recovered, 2 missing and 1 duplicate.');
  assert.equal(scanSummary(result({ copies: 2 })), 'Rescan complete: 2 duplicates.');
});

test('scanSummary for a full pass, a skip and a stop', () => {
  assert.equal(scanSummary(result({ full: true, changed: 1 })), 'Re-index complete: 1 changed.');
  assert.equal(scanSummary(result({ full: true })), 'Re-index complete: no changes.');
  assert.equal(scanSummary(result({ full: true, skipped: 'Archive/ is missing' })), 'Re-index skipped: Archive/ is missing');
  assert.equal(scanSummary(result({ skipped: 'nope', added: 5 })), 'Rescan skipped: nope');
  assert.equal(scanSummary(result({ stopped: true })), 'Rescan stopped before it finished. It carries on at the next scan.');
  assert.equal(scanSummary(result({ full: true, stopped: true })), 'Re-index stopped before it finished. It carries on at the next scan.');
});

test('scanSummary adds busy folders and problems', () => {
  assert.equal(scanSummary(result({ busy_folders: 1 })),
    'Rescan complete: no changes. 1 folder was being promoted and will be scanned next time.');
  assert.equal(scanSummary(result({ added: 1, busy_folders: 2, errors: ['a broke', 'b broke'] })),
    'Rescan complete: 1 new. 2 folders were being promoted and will be scanned next time. 2 problems: a broke');
  assert.equal(scanSummary(result({ errors: ['x'] })), 'Rescan complete: no changes. 1 problem: x');
});

test('sizeChange', () => {
  assert.equal(sizeChange(1000, 1000), 'same size');
  assert.equal(sizeChange(1000, 13300), '+12.3 KB');
  assert.equal(sizeChange(13300, 1000), '-12.3 KB');
});

test('lastScanLabel', () => {
  assert.equal(lastScanLabel(null), 'Never');
  assert.equal(lastScanLabel(undefined), 'Never');
  assert.equal(lastScanLabel({ finished_at: '2026-11-21T19:42:00Z' }), formatWhen('2026-11-21T19:42:00Z'));
});

test('backupLine', () => {
  assert.equal(backupLine(null), 'No catalog backup yet.');
  assert.equal(backupLine({ at: '2026-11-21T03:00:00Z', bytes: 1234567, error: 'disk full' }), 'The last catalog backup failed: disk full');
  assert.equal(backupLine({ at: '2026-11-21T03:00:00Z', bytes: 1234567 }),
    `Last catalog backup ${formatWhen('2026-11-21T03:00:00Z')}, 1.2 MB.`);
});
