process.env.TZ = 'Pacific/Chatham'; // +13:45 in November
import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as f from './format.js';

test('formatBytes', () => {
  assert.equal(f.formatBytes(0), '0 B');
  assert.equal(f.formatBytes(999), '999 B');
  assert.equal(f.formatBytes(1000), '1.0 KB');
  assert.equal(f.formatBytes(1234567), '1.2 MB');
  assert.equal(f.formatBytes(5e10), '50.0 GB');
  assert.equal(f.formatBytes(undefined), '0 B');
  assert.equal(f.formatBytes(999.4), '999 B');
  assert.equal(f.formatBytes(999.6), '1.0 KB');
  assert.equal(f.formatBytes(999_949), '999.9 KB');
  assert.equal(f.formatBytes(999_950), '1.0 MB');
  assert.equal(f.formatBytes(999_950_000), '1.0 GB');
  assert.equal(f.formatBytes(-5), '0 B');
  assert.equal(f.formatBytes(NaN), '0 B');
  assert.equal(f.formatBytes(Infinity), '0 B');
  assert.equal(f.formatBytes(5e15), '5000.0 TB');
});

test('formatWhen and formatCapture', () => {
  assert.equal(f.formatWhen('2026-11-21T06:42:05.000Z'), '2026-11-21 20:27');
  assert.equal(f.formatWhen(''), '');
  assert.equal(f.formatCapture('2025-05-31T01:30:12'), '2025-05-31 01:30:12');
});

test('dateRange', () => {
  assert.equal(f.dateRange([]), '');
  assert.equal(f.dateRange(['2025-05-30']), '2025-05-30');
  assert.equal(f.dateRange(['2025-06-01', '2025-05-30']), '2025-05-30 → 2025-06-01');
});

test('phaseSteps', () => {
  const states = (p) => f.phaseSteps(p).map((s) => s.state).join(',');
  assert.equal(states('queued'), 'todo,todo,todo,todo');
  assert.equal(states('scanning'), 'current,todo,todo,todo');
  assert.equal(states('copying'), 'done,done,current,todo');
  assert.equal(states('done'), 'done,done,done,done');
  assert.deepEqual(f.phaseSteps('classifying').map((s) => s.key), ['scanning', 'classifying', 'copying', 'finishing']);
});

test('progressFraction', () => {
  assert.equal(f.progressFraction(null), 0);
  assert.equal(f.progressFraction({ phase: 'classifying', files_done: 1, files_total: 4 }), 0.25);
  assert.equal(f.progressFraction({ phase: 'copying', files_done: 1, files_total: 2, bytes_done: 30, bytes_total: 40 }), 0.75);
  assert.equal(f.progressFraction({ phase: 'copying', files_done: 1, files_total: 2, bytes_done: 0, bytes_total: 0 }), 0.5);
  assert.equal(f.progressFraction({ phase: 'classifying', files_done: 0, files_total: 0 }), 0);
  assert.equal(f.progressFraction({ phase: 'finishing' }), 1);
});

test('labels and classes', () => {
  assert.equal(f.statusLabel('running'), 'Importing');
  assert.equal(f.statusLabel('refused'), 'Refused');
  assert.equal(f.levelClass('normal'), 'ok');
  assert.equal(f.levelClass('warning'), 'warn');
  assert.equal(f.levelClass('alert'), 'bad');
  assert.equal(f.statusClass({ status: 'running', level: 'normal' }), 'live');
  assert.equal(f.statusClass({ status: 'done', level: 'warning' }), 'warn');
  assert.equal(f.cardName({ card_label: 'SONY_A7IV', card_path: '/cards/X' }), 'SONY_A7IV');
  assert.equal(f.cardName({ card_label: '', card_path: '/cards/GOPRO' }), 'GOPRO');
  assert.equal(f.knownCount({ in_archive: 3, in_staging: 2, seen_before: 1 }), 6);
});

test('sessionFolder and groupKnown', () => {
  assert.equal(f.sessionFolder('Archive/2026-11-08 - Big Sur/Capture/x.ARW'), 'Archive/2026-11-08 - Big Sur');
  assert.equal(f.sessionFolder('Staging/2025-05-30/a.JPG'), 'Staging/2025-05-30');
  assert.equal(f.sessionFolder('Archive/x.JPG'), 'Archive');
  assert.equal(f.sessionFolder(''), '');
  const files = [
    { outcome: 'in_archive', rel_path: 'Archive/B/x.JPG' },
    { outcome: 'in_archive', rel_path: 'Archive/A/Capture/y.ARW' },
    { outcome: 'in_archive', rel_path: 'Archive/A/z.JPG' },
    { outcome: 'in_staging', rel_path: 'Staging/2025-05-30/a.JPG' },
    { outcome: 'seen_before' },
    { outcome: 'in_archive' },
  ];
  assert.deepEqual(f.groupKnown(files), [
    { folder: 'Archive/A', count: 2 },
    { folder: '(location unknown)', count: 1 },
    { folder: 'Archive/B', count: 1 },
    { folder: 'Staging/2025-05-30', count: 1 },
  ]);
  assert.deepEqual(f.outcomeFiles(files, 'seen_before'), [{ outcome: 'seen_before' }]);
});

test('kindChips and extLabel', () => {
  assert.deepEqual(f.kindChips({ jpg: 2, raw: 0, video: 1 }), [
    { kind: '', label: 'All', count: 3 },
    { kind: 'jpg', label: 'JPG', count: 2 },
    { kind: 'video', label: 'Video', count: 1 },
  ]);
  assert.equal(f.extLabel({ name: '2025-05-31 01.30.12.ARW' }), 'ARW');
  assert.equal(f.extLabel({ name: 'clip - 3840w - 29.970 - 12sec.mp4' }), 'MP4');
  assert.equal(f.extLabel({ name: 'noext' }), '');
});
