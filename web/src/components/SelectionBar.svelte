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
  @media (max-width: 760px) {
    .selbar { position: fixed; top: auto; left: 0; right: 0; bottom: calc(var(--tabbar) + env(safe-area-inset-bottom)); margin: 0;
      padding: 8px calc(12px + env(safe-area-inset-right)) 8px calc(12px + env(safe-area-inset-left));
      background: var(--panel); border-top: 1px solid var(--accent); border-bottom: 0; flex-wrap: nowrap; overflow-x: auto; }
    .spacer { display: none; }
    .acts { flex-wrap: nowrap; }
  }
</style>
