<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { isId } from '../lib/paths.js';
  import { cardName, formatWhen, formatBytes, formatCount, levelClass, groupKnown, outcomeFiles } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';

  let { id } = $props();
  let ingest = $state(null);
  let error = $state('');
  const MAX = 200; // longest file list shown

  let ctl = null; // the in-flight request; starting another aborts it

  // load keeps the last good data on screen if a refetch fails.
  async function load() {
    if (!isId(id)) {
      error = 'There is no such import.';
      return;
    }
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const data = await api(`/api/ingests/${id}`, { signal: mine.signal });
      ingest = data;
      error = '';
    } catch (err) {
      if (err.name === 'AbortError') return;
      error = err.status === 503 ? `Ingest is unavailable: ${err.message}` : err.message;
    }
  }

  // Load once, again when this import finishes, and again when the live
  // stream comes back after a drop (events missed meanwhile are not replayed).
  // The first connection of a fresh page is not a reconnect.
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    const f = live.finished;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen && f && String(f.id) === id;
      finishedSeen = finishedCount;
      if (!started || reconnected || finished) {
        started = true;
        load();
      }
    });
  });
  $effect(() => () => ctl?.abort());

  const files = $derived(ingest?.files ?? []);
  const groups = $derived(groupKnown(files));
  const seenBefore = $derived(outcomeFiles(files, 'seen_before'));
  const failed = $derived(outcomeFiles(files, 'failed'));
  const unsupported = $derived(outcomeFiles(files, 'unsupported'));
  const clock = $derived(ingest?.clock_details ?? []);
  const live_ = $derived(ingest && (ingest.status === 'queued' || ingest.status === 'running'));
</script>

{#snippet fileList(items, row)}
  <ul>
    {#each items.slice(0, MAX) as item, i (i)}<li>{@render row(item)}</li>{/each}
    {#if items.length > MAX}<li>…and {formatCount(items.length - MAX)} more</li>{/if}
  </ul>
{/snippet}
{#snippet pathRow(f)}<span class="mono">{f.card_path}</span>{/snippet}
{#snippet errorRow(f)}<span class="mono">{f.card_path}</span>: {f.error}{/snippet}
{#snippet clockRow(w)}<span class="mono">{w.card_path}</span>: {w.detail}{/snippet}

<p><a href="/ingest" class="btn">← Card ingest</a></p>

{#if error && !ingest}
  <div class="note bad">{error}</div>
{:else if !ingest}
  <p class="muted">Loading…</p>
{:else}
  {@const c = ingest.counts}
  {#if error}<div class="note bad" role="alert">Couldn't refresh this import: {error}</div>{/if}
  <h1>{cardName(ingest)}</h1>
  <p class="sub">
    {formatWhen(ingest.started_at)} · <span class="mono">{ingest.card_path}</span>{#if ingest.card_size} · {formatBytes(ingest.card_size)} of photos and videos{/if}
  </p>
  {#if live_}
    <LiveIngest />
  {:else}
    <div class="note {levelClass(ingest.level)}">{ingest.message}</div>
  {/if}
  <div class="stats">
    <div class="stat ok"><b>{formatCount(c.new)}</b>new → staging</div>
    <div class="stat"><b>{formatCount(c.in_archive)}</b>already in the archive</div>
    <div class="stat"><b>{formatCount(c.in_staging)}</b>already in staging</div>
    <div class="stat warn"><b>{formatCount(c.seen_before)}</b>imported before, since removed</div>
    {#if c.failed}<div class="stat bad"><b>{formatCount(c.failed)}</b>failed</div>{/if}
    {#if c.unsupported}<div class="stat warn"><b>{formatCount(c.unsupported)}</b>not imported</div>{/if}
  </div>

  {#if groups.length}
    <details>
      <summary>Where the {formatCount(c.in_archive + c.in_staging)} known files already are</summary>
      <ul>
        {#each groups as g (g.folder)}<li>{formatCount(g.count)} → <span class="mono">{g.folder}</span></li>{/each}
      </ul>
    </details>
  {/if}
  {#if seenBefore.length}
    <details>
      <summary>{formatCount(seenBefore.length)} files were imported before but are no longer kept</summary>
      <p class="muted small" style="margin:8px 0">They were culled or discarded after an earlier import, so they're skipped and deleted shots don't come back.</p>
      {@render fileList(seenBefore, pathRow)}
    </details>
  {/if}
  {#if failed.length}
    <details open>
      <summary>{formatCount(failed.length)} files failed</summary>
      {@render fileList(failed, errorRow)}
    </details>
  {/if}
  {#if unsupported.length}
    <details open>
      <summary>{formatCount(unsupported.length)} files not imported (format not supported)</summary>
      {@render fileList(unsupported, pathRow)}
    </details>
  {/if}
  {#if clock.length}
    <details open>
      <summary>{formatCount(clock.length)} suspicious capture times</summary>
      {@render fileList(clock, clockRow)}
    </details>
  {/if}
{/if}
