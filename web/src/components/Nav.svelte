<script>
  import { live } from '../lib/live.svelte.js';

  let { tab, version } = $props();
  const importing = $derived(live.current.running !== null || live.current.queued.length > 0);
</script>

<nav class="nav" aria-label="Main">
  <div class="brand"><span class="dot" class:off={!live.connected}></span>SD2NAS</div>
  <a href="/ingest" class="tab" class:active={tab === 'ingest'} aria-current={tab === 'ingest' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 2h8l4 4v15a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1z" /><path d="M9 5v3M12 5v3M15 5v3" /><path d="M12 11v7M9 15l3 3 3-3" /></svg>
    <span class="label">Card ingest</span>
    {#if importing}<span class="pill live">live</span>{/if}
  </a>
  <a href="/staging" class="tab" class:active={tab === 'staging'} aria-current={tab === 'staging' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z" /></svg>
    <span class="label">Staging</span>
  </a>
  <a href="/archive" class="tab" class:active={tab === 'archive'} aria-current={tab === 'archive' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="4" rx="1" /><path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" /><path d="M10 12h4" /></svg>
    <span class="label">Archive</span>
    {#if live.indexer.changed}<span class="pill warn">{live.indexer.changed}</span>{/if}
  </a>
  <a href="/settings" class="tab" class:active={tab === 'settings'} aria-current={tab === 'settings' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1L7 17M17 7l2.1-2.1" /></svg>
    <span class="label">Settings</span>
  </a>
  <div class="foot">
    {live.connected ? 'Live updates on' : 'Reconnecting…'}
    {#if version}<br />Version <span class="mono">{version}</span>{/if}
  </div>
</nav>

<style>
  .nav { background: var(--nav); border-right: 1px solid var(--line); padding: 18px 12px; display: flex; flex-direction: column; gap: 4px; position: sticky; top: env(safe-area-inset-top); height: 100vh; height: calc(100dvh - env(safe-area-inset-top)); }
  .brand { font-weight: 700; font-size: 16px; padding: 4px 10px 16px; display: flex; align-items: center; gap: 8px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--ok); box-shadow: 0 0 8px var(--ok); }
  .dot.off { background: var(--muted); box-shadow: none; }
  .tab { color: var(--muted); text-decoration: none; padding: 9px 12px; border-radius: 8px; display: flex; align-items: center; gap: 10px; min-height: 44px; }
  .tab:hover { background: var(--panel); color: var(--text); }
  .tab.active { background: var(--panel2); color: var(--text); }
  .label { flex: 1; }
  svg { width: 20px; height: 20px; fill: none; stroke: currentColor; stroke-width: 1.8; stroke-linecap: round; stroke-linejoin: round; flex: none; }
  .foot { margin-top: auto; font-size: 12px; color: var(--muted); padding: 10px; line-height: 1.6; }
  @media (max-width: 760px) {
    .nav { position: fixed; top: auto; left: 0; right: 0; bottom: 0; height: auto; z-index: 10; flex-direction: row; justify-content: space-around; gap: 0; padding: 6px calc(8px + env(safe-area-inset-right)) calc(6px + env(safe-area-inset-bottom)) calc(8px + env(safe-area-inset-left)); border-right: 0; border-top: 1px solid var(--line); }
    .brand, .foot { display: none; }
    .nav { width: auto; max-width: 100vw; overflow: hidden; }
    .tab { flex: 1 1 0; min-width: 0; max-width: 160px; flex-direction: column; justify-content: center; gap: 2px; padding: 6px 4px; min-height: 52px; font-size: 11px; position: relative; }
    .label { flex: none; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .tab.active { background: none; color: var(--accent); }
    .pill { position: absolute; top: 2px; left: calc(50% + 8px); }
  }
</style>
