// Selection rules shared by the folder list and the file grid (Decision 18).
// Keys are folder ids (as strings) or file hashes; `order` is the keys in
// the order they are shown.
export const LONG_PRESS_MS = 450; // a finger resting this long starts selecting
export const MOVE_SLOP = 10; // px a resting finger may drift

// What a click or tap on an item does. Ctrl/⌘ (toggle) and Shift are
// desktop modifiers; on touch, long-press is what starts selecting.
export function clickAction({ selecting = false, shift = false, toggle = false } = {}) {
  if (shift && selecting) return 'range';
  if (shift || toggle || selecting) return 'toggle';
  return 'open';
}

export function toggleKey(selected, key) {
  const out = new Set(selected);
  if (out.has(key)) out.delete(key);
  else out.add(key);
  return out;
}

export function rangeKeys(order, a, b) {
  const i = order.indexOf(a);
  const j = order.indexOf(b);
  if (i < 0 || j < 0) return [];
  return order.slice(Math.min(i, j), Math.max(i, j) + 1);
}

export function selectRange(selected, order, anchor, key) {
  const keys = rangeKeys(order, anchor, key);
  const out = new Set(selected);
  for (const k of keys.length ? keys : [key]) out.add(k);
  return out;
}

// The selection while a finger drags from anchor to over: always computed
// from base, so dragging back shrinks the range again.
export function dragSelection(base, order, anchor, over, adding) {
  const out = new Set(base);
  const keys = rangeKeys(order, anchor, over);
  for (const k of keys.length ? keys : [anchor]) {
    if (adding) out.add(k);
    else out.delete(k);
  }
  return out;
}

export function prune(selected, order) {
  const shown = new Set(order);
  const out = new Set([...selected].filter((k) => shown.has(k)));
  return out.size === selected.size ? selected : out;
}

export function autoScrollSpeed(y, height, edge = 64, max = 18) {
  if (y < edge) return -Math.min(max, Math.ceil(((edge - y) / edge) * max));
  if (y > height - edge) return Math.min(max, Math.ceil(((y - (height - edge)) / edge) * max));
  return 0;
}

// isTextEntry: whether typing goes into this element, so page shortcuts
// (Esc, Ctrl/⌘-A) must leave it alone. Checkboxes, buttons and other
// non-text controls are not text entry.
const NON_TEXT_INPUTS = new Set(['checkbox', 'radio', 'button', 'submit', 'reset', 'image', 'file', 'range', 'color']);
export function isTextEntry(el) {
  if (!el) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName?.toLowerCase();
  if (tag === 'textarea' || tag === 'select') return true;
  return tag === 'input' && !NON_TEXT_INPUTS.has((el.type || 'text').toLowerCase());
}
