<script>
  // A segmented control: one button per view, the current one aria-pressed.
  let { options, value, label, onchange } = $props(); // options: [{ value, label }]
</script>

<div class="seg" role="group" aria-label={label}>
  {#each options as o (o.value)}
    <button type="button" class:on={value === o.value} aria-pressed={value === o.value} onclick={() => onchange(o.value)}>{o.label}</button>
  {/each}
</div>

<style>
  .seg { display: inline-flex; border: 1px solid var(--line); border-radius: 8px; overflow: hidden; background: var(--panel2); flex: none; }
  button { background: none; border: 0; border-right: 1px solid var(--line); color: var(--muted); font: inherit; font-size: 13px; padding: 0 14px;
    min-height: 44px; min-width: 44px; cursor: pointer; }
  button:last-child { border-right: 0; }
  button:hover { color: var(--text); }
  button.on { background: var(--accent); color: #fff; }
</style>
