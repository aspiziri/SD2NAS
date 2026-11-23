<script>
  import { toasts, dismiss } from '../lib/toast.svelte.js';
</script>

<div class="toasts" aria-live="polite">
  {#each toasts.items as t (t.id)}
    <button class="toast {t.kind}" onclick={() => dismiss(t.id)}>{t.text}</button>
  {/each}
</div>

<style>
  .toasts { position: fixed; left: 50%; transform: translateX(-50%); bottom: calc(20px + env(safe-area-inset-bottom)); z-index: 40;
    display: flex; flex-direction: column; gap: 8px; width: min(480px, calc(100% - 32px)); pointer-events: none; }
  .toast { pointer-events: auto; border: 0; border-radius: 8px; padding: 10px 16px; font: inherit; font-weight: 600; text-align: left;
    background: var(--ok); color: #06210f; box-shadow: 0 6px 20px rgba(0,0,0,.35); min-height: 44px; cursor: pointer; }
  .toast.warn { background: var(--warn); color: #1b1b1b; }
  .toast.bad { background: var(--bad); color: #fff; }
  @media (max-width: 760px) {
    .toasts { bottom: calc(var(--tabbar) + 76px + env(safe-area-inset-bottom)); } /* above the tab bar and a selection bar */
  }
</style>
