import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { parseAmount, secondsBetween, shiftWall, shiftedName, folderDateOf, wholeFolderName, previewShift } from './shift.js';

// The table the Go test (TestShiftRuleCases) reads too: Decision 27.
const cases = JSON.parse(readFileSync(new URL('./testdata/rules-cases.json', import.meta.url), 'utf8'));

test('rule cases: shiftWall', () => {
  assert.ok(cases.shiftWall.length >= 2);
  for (const c of cases.shiftWall) assert.equal(shiftWall(c.capture, c.seconds), c.want, JSON.stringify(c));
});

test('rule cases: shiftedName', () => {
  assert.ok(cases.shiftedName.length >= 5);
  for (const c of cases.shiftedName) assert.equal(shiftedName(c.name, c.capture, c.seconds), c.want, JSON.stringify(c));
});

test('rule cases: folderDate', () => {
  assert.ok(cases.folderDate.length >= 3);
  for (const c of cases.folderDate) assert.equal(folderDateOf(c.capture, c.dayStartsAt), c.want, JSON.stringify(c));
});

test('rule cases: wholeName', () => {
  assert.ok(cases.wholeName.length >= 2);
  for (const c of cases.wholeName) assert.equal(wholeFolderName(c.name, c.dates), c.want, JSON.stringify(c));
});

const amt = (sign, days, hours, minutes, seconds) => ({ sign, days, hours, minutes, seconds });

test('parseAmount', () => {
  assert.deepEqual(parseAmount(amt('+', '0', '1', '30', '0')), { seconds: 5400 });
  assert.deepEqual(parseAmount(amt('-', '0', '1', '30', '0')), { seconds: -5400 });
  assert.deepEqual(parseAmount(amt('+', '', '', '', '45')), { seconds: 45 });
  assert.deepEqual(parseAmount(amt('+', '0', '0', '0', '0')), { error: 'Shift by at least one second.' });
  assert.deepEqual(parseAmount(amt('+', '', '', '', '')), { error: 'Shift by at least one second.' });
  assert.deepEqual(parseAmount(amt('+', '1.5', '0', '0', '0')), { error: 'Use whole numbers.' });
  assert.deepEqual(parseAmount(amt('+', '0', 'x', '0', '0')), { error: 'Use whole numbers.' });
  assert.deepEqual(parseAmount(amt('-', '36525', '0', '0', '0')), { seconds: -3155760000 });
  assert.deepEqual(parseAmount(amt('+', '36525', '0', '0', '1')), { error: 'Shift by at most 100 years.' });
  assert.deepEqual(parseAmount(amt('+', '99999999999999999999', '0', '0', '0')), { error: 'Shift by at most 100 years.' });
});

test('secondsBetween', () => {
  assert.equal(secondsBetween('2000-01-01T00:03:10', '2000-01-01T00:03:20'), 10);
  assert.equal(secondsBetween('2025-05-30T14:02:00', '2025-05-30T14:02'), 0);
  // 9281 days (2000-01-01 to 2025-05-30, 7 leap days) and 13:58:50
  assert.equal(secondsBetween('2000-01-01T00:03:10', '2025-05-30T14:02'), (9281 * 24 * 3600) + (13 * 3600 + 58 * 60 + 50));
  assert.equal(secondsBetween('2025-05-30T14:02', '2000-01-01T00:03:10'), -((9281 * 24 * 3600) + (13 * 3600 + 58 * 60 + 50)));
});

test('shiftWall ignores DST', () => {
  assert.equal(shiftWall('2025-03-09T01:30:00', 3600), '2025-03-09T02:30:00');
  assert.equal(shiftWall('2025-11-02T01:30:00', 3600), '2025-11-02T02:30:00');
});

test('folderDateOf', () => {
  assert.equal(folderDateOf('2025-05-31T03:59:59', '04:00'), '2025-05-30');
  assert.equal(folderDateOf('2025-03-01T00:30:00', '04:00'), '2025-02-28');
  assert.equal(folderDateOf('2025-01-01T00:30:00', '04:00'), '2024-12-31');
});

test('wholeFolderName', () => {
  assert.equal(wholeFolderName('2025-05-30 - Trip', ['2025-05-31', '2025-06-01']), '2025-05-31 - Trip');
  assert.equal(wholeFolderName('Untitled', ['2025-05-31']), 'Untitled');
});

test('previewShift', () => {
  const files = [
    { name: '2025-05-30 23.00.00.JPG', capture_time: '2025-05-30T23:00:00' },
    { name: '2025-05-30 10.00.00.JPG', capture_time: '2025-05-30T10:00:00' },
    { name: 'clip.MP4', capture_time: '' },
  ];
  const folder = { name: '2025-05-30 - Trip', dates: ['2025-05-30'] };
  const whole = previewShift(files, 86400, { dayStartsAt: '04:00', folder, whole: true });
  assert.equal(whole.first.to, '2025-05-31 10.00.00.JPG');
  assert.equal(whole.last.to, '2025-05-31 23.00.00.JPG');
  assert.equal(whole.skipped, 1);
  assert.equal(whole.folderName, '2025-05-31 - Trip');
  const part = previewShift(files, 3 * 3600, { dayStartsAt: '04:00', folder, whole: false });
  assert.equal(part.moved, 0);
  assert.equal(part.folderName, null);
  assert.equal(previewShift(files, 5 * 3600, { dayStartsAt: '04:00', folder, whole: false }).moved, 1);
  assert.equal(previewShift(files, 3600, { dayStartsAt: '', folder, whole: false }).moved, null);
});
