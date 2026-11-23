import { LONG_PRESS_MS, MOVE_SLOP, autoScrollSpeed } from './selection.js';

// longPressSelect: on a list or grid whose items carry data-key, a finger
// resting LONG_PRESS_MS on an item calls onlong(key). While that finger
// stays down, dragging it calls ondrag(key) for each item it crosses, the
// page does not scroll (except by auto-scroll near the screen edges), and
// lifting it calls ondragend(). The click a long press would produce is
// swallowed. Mouse and pen are ignored.
export function longPressSelect(node, params) {
  let p = params;
  let timer = 0;
  let start = null; // { x, y, key } of the resting finger
  let active = false; // the long press fired; the finger is selecting
  let swallow = false; // eat the click that ends a long press
  let lastKey = null;
  let pos = { x: 0, y: 0 };
  let raf = 0;

  const keyAt = (x, y) => document.elementFromPoint(x, y)?.closest('[data-key]')?.dataset.key ?? null;
  const cancelTimer = () => {
    clearTimeout(timer);
    timer = 0;
  };

  function onstart(e) {
    swallow = false;
    if (e.touches.length !== 1) {
      cancelTimer();
      start = null;
      return;
    }
    const item = e.target.closest?.('[data-key]');
    if (!item || !node.contains(item)) return;
    const t = e.touches[0];
    start = { x: t.clientX, y: t.clientY, key: item.dataset.key };
    cancelTimer();
    timer = setTimeout(() => {
      timer = 0;
      active = true;
      swallow = true;
      lastKey = start.key;
      pos = { x: start.x, y: start.y };
      navigator.vibrate?.(10);
      p.onlong?.(start.key);
    }, LONG_PRESS_MS);
  }

  function onmove(e) {
    const t = e.touches[0];
    if (!active) {
      if (start && Math.hypot(t.clientX - start.x, t.clientY - start.y) > MOVE_SLOP) {
        cancelTimer(); // the finger is scrolling, not resting
        start = null;
      }
      return;
    }
    e.preventDefault(); // hold the page still while selecting
    pos = { x: t.clientX, y: t.clientY };
    hit();
    if (!raf) raf = requestAnimationFrame(scrollStep);
  }

  function hit() {
    const key = keyAt(pos.x, pos.y);
    if (key && key !== lastKey) {
      lastKey = key;
      p.ondrag?.(key);
    }
  }

  function scrollStep() {
    raf = 0;
    if (!active) return;
    const v = autoScrollSpeed(pos.y, window.innerHeight);
    if (v) {
      window.scrollBy(0, v);
      hit();
      raf = requestAnimationFrame(scrollStep);
    }
  }

  function onend() {
    cancelTimer();
    start = null;
    if (active) {
      active = false;
      p.ondragend?.();
    }
  }

  function onclick(e) {
    if (swallow) {
      swallow = false;
      e.preventDefault();
      e.stopPropagation();
    }
  }

  function oncontextmenu(e) {
    if (active || timer) e.preventDefault(); // Android's long-press menu
  }

  node.addEventListener('touchstart', onstart, { passive: true });
  node.addEventListener('touchmove', onmove, { passive: false });
  node.addEventListener('touchend', onend);
  node.addEventListener('touchcancel', onend);
  node.addEventListener('click', onclick, true);
  node.addEventListener('contextmenu', oncontextmenu);
  return {
    update(next) {
      p = next;
    },
    destroy() {
      cancelTimer();
      cancelAnimationFrame(raf);
      node.removeEventListener('touchstart', onstart);
      node.removeEventListener('touchmove', onmove);
      node.removeEventListener('touchend', onend);
      node.removeEventListener('touchcancel', onend);
      node.removeEventListener('click', onclick, true);
      node.removeEventListener('contextmenu', oncontextmenu);
    },
  };
}
