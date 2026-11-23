// Decides what a finished pointer gesture on the preview means. dx and dy
// are the movement from press to release, x the release position and width
// the stage width. A gesture that involved more than one finger (a pinch)
// means nothing.
export function classifyGesture({ dx, dy, x, width, multi }) {
  if (multi) return null;
  const ax = Math.abs(dx);
  const ay = Math.abs(dy);
  if (ax > 50 && ax > ay) return dx < 0 ? 'next' : 'prev';
  if (dy > 80 && dy > ax) return 'close';
  if (ax < 10 && ay < 10) {
    const third = width / 3;
    if (x < third) return 'prev';
    if (x > 2 * third) return 'next';
    return 'toggle';
  }
  return null;
}
