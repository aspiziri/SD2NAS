<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatBytes, formatCount, dateRange } from '../lib/format.js';
  import Thumb from '../components/Thumb.svelte';

  let data = $state(null);
  let error = $state('');
  let ctl = null; // the load in flight; a newer one aborts it

  // A failed refetch keeps the last good data on screen (error shows above it).
  async function load() {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api('/api/staging', { signal: mine.signal });
      if (mine.signal.aborted) return;
      data = res;
      error = '';
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      error = err.message;
    }
  }

  // Load on mount, when an import finishes (new files may have landed), and
  // when the live stream comes back after a real drop (not on first connect).
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      if (!started || reconnected || finished) {
        started = true;
        load();
      }
    });
  });
  $effect(() => () => ctl?.abort());

  const totals = $derived.by(() => {
    const fs = data?.folders ?? [];
    return { folders: fs.length, files: fs.reduce((s, f) => s + f.files, 0), bytes: fs.reduce((s, f) => s + f.bytes, 0) };
  });
</script>

<h1>Staging</h1>
{#if error && data}
  <div class="note bad" role="alert">Couldn't refresh staging: {error}</div>
{/if}
{#if error && !data}
  <div class="note bad">Couldn't load staging: {error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  <p class="sub">{totals.folders} {totals.folders === 1 ? 'folder' : 'folders'} · {formatCount(totals.files)} files · {formatBytes(totals.bytes)}</p>
  {#if data.untracked.length}
    <div class="note warn">Not shown because SD2NAS didn't create them: {data.untracked.join(', ')}. They were probably added over the network share.</div>
  {/if}
  {#if data.folders.length === 0}
    <div class="waiting">
      <h2>Staging is empty</h2>
      <p class="muted" style="margin:0">New imports appear here.</p>
    </div>
  {:else}
    <div class="folders">
      {#each data.folders as f (f.id)}
        <a class="folder" href="/staging/{f.id}">
          <div class="name">{f.name}</div>
          <div class="muted small">{dateRange(f.dates)}</div>
          {#if f.samples.length}
            <div class="strip">
              {#each f.samples as s (s.sha256)}<Thumb sha={s.sha256} kind={s.kind} />{/each}
            </div>
          {/if}
          <div class="row small">
            <span>
              {#if f.counts.raw}<span class="tag">{f.counts.raw} RAW</span>{/if}
              {#if f.counts.jpg}<span class="tag">{f.counts.jpg} JPG</span>{/if}
              {#if f.counts.video}<span class="tag">{f.counts.video} video</span>{/if}
            </span>
            <span class="spacer"></span>
            <span class="muted">{formatCount(f.files)} files · {formatBytes(f.bytes)}</span>
          </div>
          {#each f.warnings as w}<div class="warn small">⚠ {w}</div>{/each}
        </a>
      {/each}
    </div>
  {/if}
{/if}

<style>
  .folders { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 14px; }
  .folder { display: flex; flex-direction: column; gap: 4px; background: var(--panel); border: 1px solid var(--line); border-radius: var(--r); padding: 12px; color: var(--text); text-decoration: none; min-width: 0; }
  .folder:hover { border-color: #41567a; }
  .name { font-weight: 600; overflow-wrap: anywhere; }
  .strip { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 4px; margin: 6px 0; }
  .warn { color: var(--warn); }
  @media (max-width: 760px) {
    .folders { grid-template-columns: minmax(0, 1fr); }
  }
</style>
