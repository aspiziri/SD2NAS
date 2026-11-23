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

  async function load() {
    if (!isId(id)) {
      error = 'There is no such import.';
      return;
    }
    try {
      ingest = await api(`/api/ingests/${id}`);
      error = '';
    } catch (err) {
      error = err.status === 503 ? `Ingest is unavailable: ${err.message}` : err.message;
    }
  }

  // Load once, again when this import finishes, and again when the live
  // stream reconnects (events missed while it was down are not replayed).
  let started = false;
  $effect(() => {
    const f = live.finished;
    const connected = live.connected;
    untrack(() => {
      if (!started || connected || (f && String(f.id) === id)) {
        started = true;
        load();
      }
    });
  });

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

{#if error}
  <div class="note bad">{error}</div>
{:else if !ingest}
  <p class="muted">Loading…</p>
{:else}
  {@const c = ingest.counts}
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
