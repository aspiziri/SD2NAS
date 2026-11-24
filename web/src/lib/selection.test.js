import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clickAction, toggleKey, rangeKeys, selectRange, dragSelection, prune, autoScrollSpeed } from './selection.js';

const order = ['a', 'b', 'c', 'd', 'e'];
const set = (...k) => new Set(k);

test('a click opens until something is selected, then toggles', () => {
  assert.equal(clickAction({ selecting: false }), 'open');
  assert.equal(clickAction({ selecting: true }), 'toggle');
  assert.equal(clickAction({ selecting: false, toggle: true }), 'toggle'); // Ctrl/⌘-click starts selecting
  assert.equal(clickAction({ selecting: false, shift: true }), 'toggle'); // nothing to extend yet
  assert.equal(clickAction({ selecting: true, shift: true }), 'range');
});

test('toggleKey returns a new set', () => {
  const s = set('a');
  const t = toggleKey(s, 'b');
  assert.deepEqual([...t].sort(), ['a', 'b']);
  assert.deepEqual([...s], ['a']);
  assert.deepEqual([...toggleKey(t, 'a')], ['b']);
});

test('ranges work both ways and need both ends', () => {
  assert.deepEqual(rangeKeys(order, 'b', 'd'), ['b', 'c', 'd']);
  assert.deepEqual(rangeKeys(order, 'd', 'b'), ['b', 'c', 'd']);
  assert.deepEqual(rangeKeys(order, 'x', 'b'), []);
  assert.deepEqual([...selectRange(set('a'), order, 'b', 'd')].sort(), ['a', 'b', 'c', 'd']);
  assert.deepEqual([...selectRange(set(), order, null, 'c')], ['c']);
});

test('dragging adds or removes the range from the starting selection', () => {
  const base = set('a', 'e');
  assert.deepEqual([...dragSelection(base, order, 'b', 'd', true)].sort(), ['a', 'b', 'c', 'd', 'e']);
  // Dragging back shrinks the range again: it is always computed from base.
  assert.deepEqual([...dragSelection(base, order, 'b', 'b', true)].sort(), ['a', 'b', 'e']);
  assert.deepEqual([...dragSelection(set('a', 'b', 'c'), order, 'c', 'a', false)], []);
});

test('prune drops hidden keys and keeps the same set when nothing changes', () => {
  const s = set('a', 'z');
  assert.deepEqual([...prune(s, order)], ['a']);
  const t = set('a', 'b');
  assert.equal(prune(t, order), t);
});

test('auto-scroll speeds up towards the edges', () => {
  assert.equal(autoScrollSpeed(400, 800), 0);
  assert.ok(autoScrollSpeed(10, 800) < 0);
  assert.ok(autoScrollSpeed(10, 800) < autoScrollSpeed(50, 800));
  assert.ok(autoScrollSpeed(790, 800) > 0);
  assert.equal(autoScrollSpeed(-50, 800), -18);
  assert.equal(autoScrollSpeed(900, 800), 18);
});

test('isTextEntry spots text fields but not checkboxes or buttons', async () => {
  const { isTextEntry } = await import('./selection.js');
  assert.equal(isTextEntry({ tagName: 'INPUT', type: 'text' }), true);
  assert.equal(isTextEntry({ tagName: 'INPUT' }), true);
  assert.equal(isTextEntry({ tagName: 'TEXTAREA' }), true);
  assert.equal(isTextEntry({ tagName: 'SELECT' }), true);
  assert.equal(isTextEntry({ tagName: 'DIV', isContentEditable: true }), true);
  assert.equal(isTextEntry({ tagName: 'INPUT', type: 'checkbox' }), false);
  assert.equal(isTextEntry({ tagName: 'BUTTON' }), false);
  assert.equal(isTextEntry(null), false);
});
