// Short confirmations of finished actions (the mockup's toast).
export const toasts = $state({ items: [] });
let seq = 0;

export function toast(text, kind = 'ok', ms = 4000) {
  const id = ++seq;
  toasts.items.push({ id, text, kind });
  setTimeout(() => dismiss(id), ms);
}

export function dismiss(id) {
  toasts.items = toasts.items.filter((t) => t.id !== id);
}
