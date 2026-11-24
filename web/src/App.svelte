<script>
  import { onMount } from 'svelte';
  import { route, navigate } from './lib/router.svelte.js';
  import { match } from './lib/paths.js';
  import { connectEvents } from './lib/live.svelte.js';
  import { getHealth } from './lib/api.js';
  import Toast from './components/Toast.svelte';
  import Nav from './components/Nav.svelte';
  import HealthBanner from './components/HealthBanner.svelte';
  import Ingest from './pages/Ingest.svelte';
  import IngestDetail from './pages/IngestDetail.svelte';
  import Staging from './pages/Staging.svelte';
  import Folder from './pages/Folder.svelte';
  import Archive from './pages/Archive.svelte';

  let health = $state(null);
  let healthError = $state('');

  async function refreshHealth() {
    try {
      health = await getHealth();
      healthError = '';
    } catch (err) {
      healthError = err.message;
    }
  }

  onMount(() => {
    connectEvents();
    refreshHealth();
    const timer = setInterval(refreshHealth, 60_000);
    return () => clearInterval(timer);
  });

  // Same-origin links navigate in place. Modified clicks, new tabs,
  // downloads, /api/ URLs and /healthz are left to the browser.
  function onclick(e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target instanceof Element ? e.target.closest('a[href]') : null;
    if (!a || a.target || a.hasAttribute('download')) return;
    const url = new URL(a.href);
    if (url.origin !== location.origin || url.pathname.startsWith('/api/') || url.pathname === '/healthz') return;
    e.preventDefault();
    navigate(url.pathname);
  }

  const page = $derived.by(() => {
    const p = route.path;
    if (p === '/' || p === '/ingest') return { tab: 'ingest', view: 'ingest' };
    let m = match('/ingest/:id', p);
    if (m) return { tab: 'ingest', view: 'ingest-detail', id: m.id };
    if (p === '/staging') return { tab: 'staging', view: 'staging' };
    m = match('/staging/:id', p);
    if (m) return { tab: 'staging', view: 'folder', id: m.id };
    if (p === '/archive') return { tab: 'archive', view: 'archive' };
    return { tab: '', view: 'not-found' };
  });
</script>

<svelte:document {onclick} />

<div class="app">
  <Nav tab={page.tab} version={health?.version ?? ''} />
  <main>
    <HealthBanner {health} error={healthError} />
    {#if page.view === 'ingest'}
      <Ingest />
    {:else if page.view === 'ingest-detail'}
      {#key page.id}<IngestDetail id={page.id} />{/key}
    {:else if page.view === 'staging'}
      <Staging />
    {:else if page.view === 'folder'}
      {#key page.id}<Folder id={page.id} />{/key}
    {:else if page.view === 'archive'}
      <Archive />
    {:else}
      <h1>Page not found</h1>
      <p class="sub">Nothing lives at <span class="mono">{route.path}</span>. <a href="/">Go to Card ingest</a></p>
    {/if}
  </main>
</div>
<Toast />

<style>
  .app { display: grid; grid-template-columns: 220px minmax(0, 1fr); min-height: 100vh; min-height: 100dvh; padding: env(safe-area-inset-top) env(safe-area-inset-right) 0 env(safe-area-inset-left); }
  main { padding: 22px 28px 60px; max-width: 1240px; width: 100%; }
  @media (max-width: 760px) {
    .app { grid-template-columns: minmax(0, 1fr); }
    main { padding: 16px 16px calc(var(--tabbar) + 24px + env(safe-area-inset-bottom)); }
  }
</style>
