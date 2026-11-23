import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classifyGesture } from './gesture.js';

const W = 900;
const g = (o) => classifyGesture({ dx: 0, dy: 0, x: 450, width: W, multi: false, ...o });

test('horizontal swipes step', () => {
  assert.equal(g({ dx: -120, dy: 10 }), 'next');
  assert.equal(g({ dx: 120, dy: -10 }), 'prev');
});

test('a downward swipe closes, an upward one does nothing', () => {
  assert.equal(g({ dx: 10, dy: 150 }), 'close');
  assert.equal(g({ dx: 10, dy: -150 }), null);
});

test('taps split the stage into thirds', () => {
  assert.equal(g({ x: 450 }), 'toggle');
  assert.equal(g({ x: 100 }), 'prev');
  assert.equal(g({ x: 800 }), 'next');
});

test('a small diagonal move is nothing', () => {
  assert.equal(g({ dx: 30, dy: 30 }), null);
});

test('multi-touch gestures are ignored', () => {
  for (const o of [{ dx: -120 }, { dx: 120 }, { dy: 150 }, { x: 450 }, { x: 100 }, { x: 800 }]) {
    assert.equal(g({ ...o, multi: true }), null, JSON.stringify(o));
  }
});
