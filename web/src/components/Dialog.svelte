<script>
  // A modal on the native <dialog>. Escape and a click on the backdrop
  // close it, except while busy (a request is running).
  let { open = false, title, onclose, busy = false, danger = false, children, actions } = $props();
  let el;
  const id = `dlg-${Math.random().toString(36).slice(2)}`;

  $effect(() => {
    if (open && !el.open) el.showModal();
    else if (!open && el.open) el.close();
  });

  function oncancel(e) {
    e.preventDefault(); // Escape: we decide
    if (!busy) onclose();
  }
  // The browser can close a modal on its own: Chrome's close watcher lets a
  // second Escape through even after cancel was prevented. Keep `open` in
  // step: tell the parent, or show it again while a request runs.
  function onnativeclose() {
    if (!open) return;
    if (busy) el.showModal();
    else onclose();
  }
  function onbackdrop(e) {
    if (e.target === el && !busy) onclose(); // the backdrop, not the box
  }
</script>

<!-- Escape is handled by the cancel event; the click is only the backdrop shortcut. -->
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
<dialog bind:this={el} {oncancel} onclose={onnativeclose} onclick={onbackdrop} aria-labelledby={id} class:danger>
  <div class="box">
    <h2 {id}>{title}</h2>
    {@render children?.()}
    <div class="actions">{@render actions?.()}</div>
  </div>
</dialog>

<style>
  dialog { padding: 0; border: 1px solid var(--line); border-radius: 12px; background: var(--panel); color: var(--text);
    width: min(520px, calc(100% - 32px)); max-height: calc(100dvh - 32px - env(safe-area-inset-top) - env(safe-area-inset-bottom)); }
  dialog::backdrop { background: rgba(5, 9, 15, .65); }
  .box { padding: 20px; overflow: auto; }
  h2 { margin: 0 0 12px; }
  .danger h2 { color: var(--bad); }
  .actions { display: flex; gap: 10px; justify-content: flex-end; flex-wrap: wrap; margin-top: 18px; }
  @media (max-width: 760px) {
    dialog { width: 100%; max-width: none; margin: auto 0 0; border-radius: 14px 14px 0 0; border-bottom: 0; }
    .box { padding: 18px 16px calc(18px + env(safe-area-inset-bottom)); }
    .actions { flex-direction: column-reverse; }
    .actions :global(.btn) { justify-content: center; width: 100%; }
  }
</style>
