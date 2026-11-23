<script>
  import { onMount, untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { isId } from '../lib/paths.js';
  import { formatBytes, formatCount, dateRange, kindChips, extLabel } from '../lib/format.js';
  import Thumb from '../components/Thumb.svelte';
  import Lightbox from '../components/Lightbox.svelte';

  let { id } = $props();
  let kind = $state(''); // the chip asked for; '' = all
  let data = $state(null);
  let shownKind = $state(''); // the kind `data` was loaded for
  let error = $state('');
  let open = $state(-1); // index into data.files shown full screen; -1 = none
  let ctl = null; // the load in flight; a newer one aborts it

  // Switching chips quickly must not let an older, slower answer land last.
  // A failed load keeps the last good data on screen (error shows above it).
  async function load(k) {
    if (!isId(id)) {
      error = 'There is no such staging folder.';
      return;
    }
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api(`/api/staging/${id}/files` + (k ? `?kind=${k}` : ''), { signal: mine.signal });
      if (mine.signal.aborted) return;
      // Keep the preview on the same file if the list changed under it.
      const sha = open >= 0 ? data?.files[open]?.sha256 : null;
      data = res;
      shownKind = k;
      error = '';
      if (open >= 0) {
        const j = res.files.findIndex((x) => x.sha256 === sha);
        if (j >= 0) open = j;
        else hide();
      }
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      error = err.message;
    }
  }

  // Load on mount, when the chip changes, when an import finishes, and when
  // the live stream comes back after a real drop (not on the first connect).
  let started = false;
  let lastKind = '';
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const k = kind;
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      const changed = k !== lastKind;
      lastKind = k;
      if (!started || changed || reconnected || finished) {
        started = true;
        load(k);
      }
    });
  });
  $effect(() => () => ctl?.abort());

  // After a reload with the preview open, this page sits on the preview's
  // history entry with nothing shown, so the first Back would seem to do
  // nothing. Step back to the folder's own entry (same URL; popstate below
  // keeps the preview closed), so the first Back leaves the folder.
  onMount(() => {
    if (history.state?.lightbox) history.back();
  });

  // The preview takes a history entry, so the phone's back gesture closes
  // it instead of leaving the folder.
  function show(i) {
    if (open >= 0) return; // already open: never push a second history entry
    open = i;
    history.pushState({ lightbox: true }, '');
  }
  function hide() {
    if (history.state?.lightbox) history.back(); // popstate below closes it
    else open = -1;
  }
  function onpopstate() {
    if (!history.state?.lightbox) open = -1;
  }
</script>

<svelte:window {onpopstate} />

<p><a href="/staging" class="btn">← Staging</a></p>

{#if error && !data}
  <div class="note bad">{error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  {@const f = data.folder}
  <h1>{f.name}</h1>
  <p class="sub">{dateRange(f.dates)} · {formatCount(f.files)} files · {formatBytes(f.bytes)}</p>
  {#if error}
    <div class="note bad" role="alert">Couldn't refresh this folder: {error}</div>
  {/if}
  {#each f.warnings as w}<div class="note warn">{w}</div>{/each}
  <div class="toolbar">
    <div class="chips">
      {#each kindChips(f.counts) as c (c.kind)}
        <button class="chip" class:on={shownKind === c.kind} onclick={() => (kind = c.kind)}>{c.label} {c.count}</button>
      {/each}
    </div>
  </div>
  {#if data.files.length === 0}
    <p class="muted">No files here.</p>
  {:else}
    <div class="grid">
      {#each data.files as file, i (file.sha256)}
        <button class="tile" onclick={() => show(i)} aria-label="Show {file.name}">
          <Thumb sha={file.sha256} kind={file.kind} label={extLabel(file)} />
          <span class="fn">{#if file.clock_warnings?.length}<span class="warn" title={file.clock_warnings.join('; ')}>⚠ </span>{/if}{file.name}</span>
        </button>
      {/each}
    </div>
  {/if}
  {#if open >= 0 && open < data.files.length}
    <Lightbox files={data.files} bind:index={open} onclose={hide} />
  {/if}
{/if}

<style>
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 8px; }
  .tile { background: none; border: 0; padding: 0; color: inherit; font: inherit; text-align: left; cursor: pointer; min-width: 0; border-radius: 6px; }
  .tile:hover { outline: 2px solid var(--accent); outline-offset: 1px; }
  .fn { display: block; font-size: 11px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-top: 3px; }
  .warn { color: var(--warn); }
  @media (max-width: 760px) {
    .grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 4px; }
  }
</style>
