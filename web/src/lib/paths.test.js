import { test } from 'node:test';
import assert from 'node:assert/strict';
import { match, isId } from './paths.js';

test('match', () => {
  assert.deepEqual(match('/staging/:id', '/staging/3'), { id: '3' });
  assert.deepEqual(match('/ingest/:id', '/ingest/a%20b'), { id: 'a b' });
  assert.equal(match('/staging/:id', '/staging'), null);
  assert.equal(match('/staging/:id', '/staging/'), null);
  assert.equal(match('/staging/:id', '/staging/3/x'), null);
  assert.equal(match('/staging/:id', '/ingest/3'), null);
  assert.equal(match('/staging/:id', '/staging/%E0'), null); // malformed escape
});

test('isId', () => {
  assert.equal(isId('1'), true);
  assert.equal(isId('42'), true);
  for (const s of ['0', '-1', '01', '1.5', 'abc', '', undefined]) assert.equal(isId(s), false, String(s));
});
