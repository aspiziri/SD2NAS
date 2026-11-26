import { test, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { loadPref, savePref, PATTERN_KEY, STAGING_VIEW_KEY, FOLDER_VIEW_KEY } from './prefs.js';

const real = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
function stub(impl) {
  Object.defineProperty(globalThis, 'localStorage', { value: impl, configurable: true });
}
afterEach(() => {
  if (real) Object.defineProperty(globalThis, 'localStorage', real);
  else delete globalThis.localStorage;
});
const memory = (init = {}) => {
  const m = { ...init };
  return { getItem: (k) => (k in m ? m[k] : null), setItem: (k, v) => { m[k] = String(v); }, m };
};

test('the keys', () => {
  assert.equal(PATTERN_KEY, 'sd2nas.promotePattern');
  assert.equal(typeof STAGING_VIEW_KEY, 'string');
  assert.equal(typeof FOLDER_VIEW_KEY, 'string');
  assert.notEqual(STAGING_VIEW_KEY, FOLDER_VIEW_KEY);
});

test('loadPref returns an allowed stored value, else the fallback', () => {
  stub(memory({ a: 'list', b: 'bogus' }));
  assert.equal(loadPref('a', ['cards', 'list'], 'cards'), 'list');
  assert.equal(loadPref('b', ['cards', 'list'], 'cards'), 'cards');
  assert.equal(loadPref('missing', ['cards', 'list'], 'cards'), 'cards');
});

test('loadPref without an allowed list takes any non-empty value', () => {
  stub(memory({ p: '{year}/{name}', e: '' }));
  assert.equal(loadPref('p', null, '{name}'), '{year}/{name}');
  assert.equal(loadPref('e', null, '{name}'), '{name}');
});

test('storage that throws gives the fallback and does not throw', () => {
  stub({ getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } });
  assert.equal(loadPref('a', ['x'], 'x'), 'x');
  assert.equal(savePref('a', 'x'), false);
});

test('savePref stores and reports success', () => {
  const m = memory();
  stub(m);
  assert.equal(savePref('a', 'list'), true);
  assert.equal(m.m.a, 'list');
});
