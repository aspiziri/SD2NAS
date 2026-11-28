<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { isId } from '../lib/paths.js';
  import { cardName, formatWhen, formatBytes, formatCount, levelClass, groupKnown, outcomeFiles } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';
  import { navigate } from '../lib/router.svelte.js';
  import { describeError, plural, missingCopies } from '../lib/messages.js';
  import { toast } from '../lib/toast.svelte.js';
  import Dialog from '../components/Dialog.svelte';

  let { id } = $props();
  let ingest = $state(null);
  let error = $state('');
  const MAX = 200; // longest file list shown

  let forget = $state(null); // GET /api/ingest/{id}/forget, for finished imports
  let dialog = $state(''); // '' | 'forget' | 'reimport' | 'reimportMissing'
  // Missing files not yet asked for: what the re-import button would request.
  const toRequest = $derived(Math.max(0, (forget?.restorable ?? 0) - (forget?.restore_requested ?? 0)));
  let busy = $state(false);
  let dialogError = $state('');
  let deleteFiles = $state(false); // Decision 10: starts unticked
  // Deleting only means something while files are still in staging (the count can drop to 0 with the dialog open).
  const del = $derived(deleteFiles && (forget?.staged ?? 0) > 0);

  // What forgetting would do; fetched after every successful load of a
  // finished import. A failure only hides the two actions: the page itself
  // already shows API trouble.
  async function loadForget() {
    try {
      forget = await api(`/api/ingest/${id}/forget`);
    } catch (err) {
      forget = null;
      console.warn('could not load what forgetting this import would do', err);
    }
  }

  const importing = $derived(live.current.running !== null || live.current.queued.length > 0);

  async function run(fn) {
    busy = true;
    dialogError = '';
    try {
      await fn();
      dialog = '';
    } catch (err) {
      dialogError = describeError(err);
    } finally {
      busy = false;
      load();
    }
  }

  function doForget() {
    run(async () => {
      const res = await api(`/api/ingest/${id}/forget`, { method: 'POST', body: { deleteFiles: del } });
      const r = res.result;
      toast(del
        ? `Forgot this import and deleted ${plural(r.deleted_files, 'file', 'files')} from staging. The card imports them again next time.`
        : (forget?.staged ?? 0) > 0
          ? `Forgot this import. Its files stay in staging untracked until the next import or rescan.`
          : `Forgot this import. The card imports its culled files again next time.`, 'ok', 7000);
    });
  }

  function doReimport() {
    run(async () => {
      const res = await api(`/api/ingest/${id}/reimport-culled`, { method: 'POST' });
      toast(res.message, res.forgotten ? 'ok' : 'warn', 7000);
      if (res.started) navigate('/ingest');
    });
  }

  function doReimportMissing() {
    run(async () => {
      const res = await api(`/api/ingest/${id}/reimport-missing`, { method: 'POST' });
      toast(res.message, res.requested ? 'ok' : 'warn', 7000);
      if (res.started) navigate('/ingest');
    });
  }

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
      if (data.status !== 'queued' && data.status !== 'running') loadForget();
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
  const missing = $derived(outcomeFiles(files, 'missing'));
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
{#snippet missingRow(f)}<span class="mono">{f.card_path}</span> → <span class="mono">{f.rel_path}</span>{/snippet}
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
  {#if ingest.forgotten_at}<span class="pill">Forgotten {formatWhen(ingest.forgotten_at)}</span>{/if}
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
    {#if c.missing}<div class="stat bad"><b>{formatCount(c.missing)}</b>archive copies missing</div>{/if}
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
      {#if forget?.reimportable}
        <button class="btn" disabled={importing} onclick={() => { dialogError = ''; dialog = 'reimport'; }}>Re-import these anyway</button>
        {#if importing}<span class="muted small">Wait for the running import to finish.</span>{/if}
      {/if}
      {@render fileList(seenBefore, pathRow)}
    </details>
  {/if}
  {#if missing.length}
    <details open>
      <summary>{missingCopies(missing.length)}</summary>
      <p class="muted small" style="margin:8px 0">SD2NAS knows these files, but their copies in the archive are gone, so the card isn't safe to format. Keep it until you've found or restored them; the Archive index page lists the missing files.</p>
      {#if forget && forget.restore_requested > 0}
        <p class="small" style="margin:8px 0">{plural(forget.restore_requested, 'file', 'files')} will be imported again the next time you insert the card.</p>
      {/if}
      {#if forget && toRequest > 0}
        <button class="btn" disabled={importing} onclick={() => { dialogError = ''; dialog = 'reimportMissing'; }}>Re-import {plural(toRequest, 'missing file', 'missing files')}…</button>
        {#if importing}<span class="muted small">Wait for the running import to finish.</span>{/if}
      {/if}
      {@render fileList(missing, missingRow)}
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

{#if ingest}
  {#if forget && !forget.forgotten}
    <div class="card forget">
      <h2>Forget this import</h2>
      <p class="muted small" style="margin:0 0 10px">SD2NAS forgets the files this import brought in or skipped as culled, so inserting the same card imports them again (to retest a card, say). Files already in the archive stay known, and the import stays in this history.</p>
      <button class="btn danger" disabled={importing} onclick={() => { dialogError = ''; deleteFiles = false; dialog = 'forget'; }}>Forget this import…</button>
      {#if importing}<span class="muted small">Wait for the running import to finish.</span>{/if}
    </div>
  {/if}
{/if}

<Dialog open={dialog === 'forget'} title="Forget this import?" onclose={() => !busy && (dialog = '')} {busy} danger>
  {#if forget}
    <ul class="muted list">
      <li>{plural(forget.staged, 'file', 'files')} still in staging ({formatBytes(forget.staged_bytes)})</li>
      <li>{plural(forget.culled, 'culled file', 'culled files')}</li>
      <li>{plural(forget.archived, 'file', 'files')} already in the archive stay known</li>
      {#if forget.missing > 0}<li>{plural(forget.missing, 'file', 'files')} whose archive copy is missing stay known</li>{/if}
    </ul>
    {#if forget.staged}
      <label class="toggle"><input type="checkbox" bind:checked={deleteFiles} /> <span>Also delete the {plural(forget.staged, 'file', 'files')} still in staging</span></label>
    {/if}
    <div class="note warn">{del
      ? 'The files are deleted from staging and SD2NAS forgets them. The next time this card goes in, they are imported again as new.'
      : forget.staged > 0
        ? 'SD2NAS forgets these files. Their copies stay in staging, untracked, until the next import of this card or a rescan picks them up again.'
        : 'Nothing of this import is left in staging. SD2NAS forgets its culled files, so the card brings them back the next time it goes in.'}</div>
  {/if}
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
    <button class="btn bad" onclick={doForget} disabled={busy}>{busy ? 'Forgetting…' : del ? `Forget and delete ${plural(forget?.staged ?? 0, 'file', 'files')}` : 'Forget'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'reimport'} title="Re-import {plural(forget?.reimportable ?? 0, 'file', 'files')}?" onclose={() => !busy && (dialog = '')} {busy}>
  <p class="muted">SD2NAS forgets that you culled {forget?.reimportable === 1 ? 'this file' : 'these files'}. If the card is still inserted they are imported right away; otherwise they come back the next time it goes in.</p>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doReimport} disabled={busy}>{busy ? 'Working…' : 'Re-import'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'reimportMissing'} title="Re-import {plural(toRequest, 'missing file', 'missing files')}?" onclose={() => !busy && (dialog = '')} {busy}>
  <p class="muted">SD2NAS copies these files from the card into Staging again. If an array disk dropped out, fix that first: its files come back by themselves at the next archive scan.</p>
  <p class="muted">If the card is still in the reader, this starts right away; otherwise the files are copied the next time it is inserted.</p>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doReimportMissing} disabled={busy}>{busy ? 'Working…' : 'Re-import'}</button>
  {/snippet}
</Dialog>

<style>
  .list { margin: 0 0 10px; padding-left: 18px; }
  .toggle { display: flex; gap: 10px; align-items: center; min-height: 44px; cursor: pointer; }
  .forget { margin-top: 16px; }
</style>
