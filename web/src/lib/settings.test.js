import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { FIELDS, changedKeys, fieldOfError, checkField } from './settings.js';

const cases = JSON.parse(readFileSync(new URL('./testdata/rules-cases.json', import.meta.url), 'utf8')).settings;

test('changedKeys: only the keys whose value differs', () => {
  assert.deepEqual(changedKeys({ a: '1', b: '2' }, { a: '1', b: '3' }), { b: '3' });
  assert.deepEqual(changedKeys({ a: '1', b: '2' }, { a: '1', b: '2' }), {});
});

test('fieldOfError names the field a server sentence is about', () => {
  assert.equal(fieldOfError('Setting staging_notice_at: want HH:MM, 00:00 to 23:59.'), 'staging_notice_at');
  assert.equal(fieldOfError('Setting backup_dir: pick a folder outside Archive/ and Staging/.'), 'backup_dir');
  assert.equal(fieldOfError('Unknown setting x.'), 'x');
  assert.equal(fieldOfError('Name at least one setting.'), '');
  assert.equal(fieldOfError('Setting x must be a string.'), '');
  assert.equal(fieldOfError(''), '');
});

test('checkField: time keys need HH:MM', () => {
  for (const k of ['day_starts_at', 'archive_rescan_at', 'staging_notice_at']) {
    assert.equal(checkField(k, '09:00'), '');
    assert.notEqual(checkField(k, '9:00'), '');
    assert.notEqual(checkField(k, '24:00'), '');
  }
});

test('checkField: numbers and paths', () => {
  assert.equal(checkField('staging_warn_days', '0'), '');
  assert.equal(checkField('staging_warn_days', '3650'), '');
  assert.notEqual(checkField('staging_warn_days', '3651'), '');
  assert.notEqual(checkField('staging_warn_days', '-1'), '');
  assert.notEqual(checkField('free_space_reserve_gb', '1.5'), '');
  assert.notEqual(checkField('free_space_reserve_gb', ''), '');
  assert.equal(checkField('backup_dir', '/mnt/user/x'), '');
  assert.equal(checkField('backup_dir', String.raw`C:\x`), '');
  assert.notEqual(checkField('backup_dir', 'x/y'), '');
  assert.equal(checkField('unknown_key', 'anything'), '');
});

test('FIELDS lists the eight catalog keys', () => {
  assert.equal(Object.keys(FIELDS).length, 8);
});

test('checkField agrees with the shared rules table', () => {
  assert.ok(cases.length > 0);
  for (const c of cases) {
    assert.equal(checkField(c.key, c.value) === '', c.ok, `${c.key} = ${JSON.stringify(c.value)}`);
  }
});

test('checkField: backup_dir drive letters need a backslash; archive_skip rejects slashes inside a name', () => {
  assert.notEqual(checkField('backup_dir', 'C:x'), '');
  assert.equal(checkField('archive_skip', 'Output/, *.xmp'), '');
  assert.notEqual(checkField('archive_skip', 'a/b'), '');
});
