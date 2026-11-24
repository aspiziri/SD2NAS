<script>
  // While anything is selected: a sticky toolbar under the page header on a
  // desktop, a bar above the tab bar on a phone (Global Constraints). On a
  // phone it also gives the page more bottom padding (body.selecting, in
  // app.css), so the last row scrolls clear of the bar (pre-flight D4).
  let { count, total, onclear, onall, children } = $props();
  $effect(() => {
    document.body.classList.add('selecting');
    return () => document.body.classList.remove('selecting');
  });
</script>

<div class="selbar" role="toolbar" aria-label="Selection">
  <span class="count">{count} selected</span>
  {#if count < total}<button class="btn link" onclick={onall}>Select all</button>{/if}
  <button class="btn link" onclick={onclear}>Clear</button>
  <span class="spacer"></span>
  <div class="acts">{@render children()}</div>
</div>

<style>
  .selbar { position: sticky; top: env(safe-area-inset-top); z-index: 6; display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
    background: var(--bg); padding: 10px 0; margin-bottom: 10px; border-bottom: 1px solid var(--accent); }
  .count { font-weight: 600; }
  .acts { display: flex; gap: 8px; flex-wrap: wrap; }
  /* Phone: two rows (count, Select all, Clear / the actions, sharing the width),
     so no action is ever scrolled out of sight. The bar is about 112 px tall;
     app.css (body.selecting main) and Toast.svelte leave room for that. */
  @media (max-width: 760px) {
    .selbar { position: fixed; top: auto; left: 0; right: 0; bottom: calc(var(--tabbar) + env(safe-area-inset-bottom)); margin: 0;
      padding: 8px calc(12px + env(safe-area-inset-right)) 8px calc(12px + env(safe-area-inset-left));
      background: var(--panel); border-top: 1px solid var(--accent); border-bottom: 0; gap: 0 10px; }
    .spacer { display: none; }
    .acts { flex: 1 0 100%; flex-wrap: nowrap; gap: 6px; }
    .acts :global(.btn) { flex: 1 1 auto; min-width: 0; justify-content: center; padding-left: 8px; padding-right: 8px; }
  }
</style>
