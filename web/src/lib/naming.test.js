import { test } from 'node:test';
import assert from 'node:assert/strict';
import { splitName, folderName, folderDate, earliestDate, nameProblem, checkFolderName, promoteTarget, MAX_NAME_BYTES } from './naming.js';

test('splitName reads the archive convention as text', () => {
  assert.deepEqual(splitName('2025-05-30 - Trip'), { date: '2025-05-30', title: 'Trip' });
  assert.deepEqual(splitName('2025-05-30'), { date: '2025-05-30', title: '' });
  assert.deepEqual(splitName('2007-09-31 - Random Party'), { date: '2007-09-31', title: 'Random Party' });
  assert.deepEqual(splitName('Holiday'), { date: '', title: 'Holiday' });
  assert.deepEqual(splitName('2025-05-30 - A - B'), { date: '2025-05-30', title: 'A - B' });
});

test('folderName joins date and title', () => {
  assert.equal(folderName('2025-05-30', 'Trip'), '2025-05-30 - Trip');
  assert.equal(folderName('2025-05-30', ''), '2025-05-30');
  assert.equal(folderName('', 'Trip'), 'Trip');
});

test('folderDate and earliestDate', () => {
  assert.equal(folderDate({ name: '2025-05-30 - Trip', dates: ['2025-05-29'] }), '2025-05-30');
  assert.equal(folderDate({ name: 'Holiday', dates: ['2025-06-01', '2025-06-02'] }), '2025-06-01');
  assert.equal(folderDate({ name: 'Holiday', dates: [] }), '');
  assert.equal(earliestDate([{ name: '2025-05-31', dates: [] }, { name: 'X', dates: ['2025-05-30'] }, { name: 'Y', dates: [] }]), '2025-05-30');
  assert.equal(earliestDate([{ name: 'Y', dates: [] }]), '');
});

test('nameProblem mirrors the server rules', () => {
  assert.equal(nameProblem('Zürich'), '');
  for (const bad of ['a/b', 'a\\b', 'a:b', 'a*b', 'a?b', 'a"b', 'a<b', 'a>b', 'a|b', 'a\tb', '.hidden', 'trailing.', 'trailing ', '', '.', '..']) {
    assert.notEqual(nameProblem(bad), '', bad);
  }
  assert.equal(nameProblem('é'.repeat(MAX_NAME_BYTES / 2)), '');
  assert.match(nameProblem('é'.repeat(MAX_NAME_BYTES / 2 + 1)), /longer than 200 bytes/);
  assert.match(nameProblem('a\ud800b'), /not valid UTF-8/);
});

test('checkFolderName trims and drops a dangling separator like CleanName', () => {
  assert.deepEqual(checkFolderName(' 2025-06-02 - Trip '), { name: '2025-06-02 - Trip', error: '' });
  assert.deepEqual(checkFolderName('2025-06-02 - '), { name: '2025-06-02', error: '' });
  assert.deepEqual(checkFolderName('2025-06-02 -'), { name: '2025-06-02', error: '' });
  assert.deepEqual(checkFolderName('2025-06-02 - A-'), { name: '2025-06-02 - A-', error: '' });
  assert.deepEqual(checkFolderName('   '), { name: '', error: '' });
  assert.match(checkFolderName('2025-06-02 - a/b').error, /^The folder name contains "\/"/);
  assert.match(checkFolderName('x'.repeat(MAX_NAME_BYTES + 1)).error, /longer than 200 bytes/);
});

test('promoteTarget expands the tokens like the server', () => {
  const f = { name: '2025-05-30 - Trip', dates: ['2025-05-30'] };
  assert.deepEqual(promoteTarget(f, ''), { path: '2025-05-30 - Trip', error: '' });
  assert.deepEqual(promoteTarget(f, '{name}'), { path: '2025-05-30 - Trip', error: '' });
  assert.deepEqual(promoteTarget(f, '{year}/{month}/{name}'), { path: '2025/05/2025-05-30 - Trip', error: '' });
  assert.match(promoteTarget(f, '{day}').error, /Unknown token \{day\}/);
  assert.match(promoteTarget(f, '{year}/{name').error, /braces/);
  assert.notEqual(promoteTarget(f, '../{name}').error, '');
  assert.notEqual(promoteTarget(f, '{year}//{name}').error, '');
  assert.match(promoteTarget({ name: 'Holiday', dates: [] }, '{year}').error, /needs a date/);
  assert.notEqual(promoteTarget({ name: 'What?', dates: [] }, '{name}').error, '');
});

test('promoteTarget rejects a malformed date like ExpandPattern', () => {
  assert.match(promoteTarget({ name: 'Holiday', dates: ['2025-6'] }, '{year}').error, /needs a YYYY-MM-DD date/);
  assert.deepEqual(promoteTarget({ name: 'Holiday', dates: ['2025-6'] }, '{name}'), { path: 'Holiday', error: '' });
});
