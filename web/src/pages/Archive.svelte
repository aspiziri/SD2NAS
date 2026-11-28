<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatBytes, formatCount, formatWhen } from '../lib/format.js';
  import { describeError, plural } from '../lib/messages.js';
  import { scanPercent, scanLine, progressDetail, scanSummary, sizeChange, lastScanLabel, backupLine, changedHeading } from '../lib/archive.js';
  import { toast } from '../lib/toast.svelte.js';
  import Dialog from '../components/Dialog.svelte';

  const MAX_ERRORS = 20; // problems listed from the last scan
  const MAX_MISSING = 50; // the server sends at most this many

  let stats = $state(null);
  let changes = $state(null); // GET /api/archive/changed, only fetched while stats.changed > 0
  let error = $state('');
  let ctl = null; // the load in flight; a newer one aborts it

  let dialog = $state(false); // the Re-index everything dialog
  let busy = $state(false); // a pause, resume or scan request is running
  let dialogError = $state('');
  let accepting = $state(false);
  let acceptError = $state('');
  let ignoring = $state(''); // sha256 of the file an ignore or un-ignore request is for
  let scanAsked = false; // this page started the pass that is running
  let scanSeen = null; // finished_at of the last pass already handled

  // load keeps the last good data on screen if a refetch fails. When the
  // answer's version is not the live one (the pass ended while it was in
  // flight), it loads once more.
  async function load(again = true) {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const s = await api('/api/archive/stats', { signal: mine.signal });
      const c = s.changed > 0 ? await api('/api/archive/changed', { signal: mine.signal }) : null;
      if (mine.signal.aborted) return;
      stats = s;
      changes = c;
      error = '';
      const v = live.indexer.version;
      if (again && v !== 0 && s.version !== v) load(false);
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      error = describeError(err);
    }
  }

  // Load on mount, when the stream comes back after a real drop, and when
  // indexer.progress carries a version other than the one on screen.
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  $effect(() => {
    const connected = live.connected;
    const version = live.indexer.version;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const stale = stats !== null && version !== 0 && version !== stats.version;
      if (!started || reconnected || stale) {
        started = true;
        load();
      }
    });
  });
  $effect(() => () => ctl?.abort());

  // Report a pass this page started.
  $effect(() => {
    const r = live.indexer.last;
    untrack(() => {
      if (!r || r.finished_at === scanSeen) return;
      scanSeen = r.finished_at;
      if (!scanAsked) return;
      scanAsked = false;
      toast(scanSummary(r), r.skipped || r.stopped || r.errors?.length ? 'warn' : 'ok', 6000);
    });
  });

  // The Status the page shows comes from the stream once it has spoken, and
  // from the loaded stats before that.
  const status = $derived(live.indexer.version !== 0 ? live.indexer : stats);
  const running = $derived(live.indexer.running);
  const last = $derived(status?.last ?? null);
  const percent = $derived(running ? scanPercent(running) : null);
  const userPaused = $derived(running?.paused_by === 'user');
  const problems = $derived(last?.errors ?? []);
  const missingFiles = $derived(stats?.missing_files ?? []);
  const ignoredFiles = $derived(stats?.ignored_files ?? []);
  const changeList = $derived(changes?.changes ?? []);
  const datePart = (iso) => formatWhen(iso).slice(0, 10);
  const bareFolder = (f) => f.replace(/^Archive\//, '');

  async function control(action) {
    busy = true;
    try {
      await api(`/api/archive/${action}`, { method: 'POST' });
    } catch (err) {
      toast(err.status === 409 ? err.message : describeError(err), 'warn', 6000);
      load();
    } finally {
      busy = false;
    }
  }

  async function rescan(full) {
    busy = true;
    dialogError = '';
    try {
      const res = await api('/api/archive/rescan', { method: 'POST', body: full ? { full: true } : {} });
      if (res?.started) scanAsked = true;
      else toast('A scan is already running.', 'warn');
      dialog = false;
    } catch (err) {
      if (full) dialogError = describeError(err);
      else toast(describeError(err), 'warn', 6000);
    } finally {
      busy = false;
    }
  }

  async function accept() {
    accepting = true;
    acceptError = '';
    try {
      await api('/api/archive/changed/accept', { method: 'POST', body: { upTo: changes.latest_id } });
      toast('Marked as reviewed. The old hashes stay as aliases, so the originals on a card still count as duplicates.', 'ok', 6000);
      load();
    } catch (err) {
      acceptError = describeError(err);
    } finally {
      accepting = false;
    }
  }

  // setIgnored ignores (or un-ignores) one missing file.
  async function setIgnored(f, ignore) {
    ignoring = f.sha256;
    try {
      await api(`/api/archive/missing/${ignore ? 'ignore' : 'unignore'}`, { method: 'POST', body: { sha256: f.sha256 } });
      toast(ignore ? 'Ignored. Cards that still hold it are safe to format.' : 'Missing again. Cards that still hold it are not safe to format.', 'ok', 6000);
    } catch (err) {
      toast(err.status === 409 ? err.message : describeError(err), 'warn', 6000);
    } finally {
      ignoring = '';
      load();
    }
  }
</script>

{#if error && !stats}
  <h1>Archive index</h1>
  <div class="note bad" role="alert">{error}</div>
{:else if !stats}
  <h1>Archive index</h1>
  <p class="muted">Loading…</p>
{:else}
  {#if error}<div class="note bad" role="alert">Couldn't refresh the archive index: {error}</div>{/if}
  <h1>Archive index</h1>
  <p class="sub">Hash catalog of <span class="mono">{stats.archive_path}</span>, used to skip files you already have.</p>
  {#if !stats.archive_present}
    <div class="note warn">Archive/ isn't there. Is the photos share mounted?</div>
  {/if}

  <div class="card">
    <div class="stats">
      <div class="stat"><b>{formatCount(stats.files)}</b>files indexed</div>
      <div class="stat"><b>{formatBytes(stats.bytes)}</b>archive size</div>
      <div class="stat"><b>{formatCount(stats.imported)}</b>files in ingest history</div>
      <div class="stat"><b class="when">{lastScanLabel(last)}</b>last scan</div>
    </div>

    {#if running}
      <div class="progress">
        <div class="bar" role="progressbar" aria-label="Scan progress" aria-valuemin="0" aria-valuemax="100" aria-valuenow={percent ?? undefined}>
          <i class:indeterminate={percent === null} style:width={percent === null ? undefined : `${percent}%`}></i>
        </div>
        <div class="line">{scanLine(running)}</div>
        {#if progressDetail(running)}<div class="muted small">{progressDetail(running)}</div>{/if}
        {#if running.current || running.folder}<div class="mono muted path">{running.current || running.folder}</div>{/if}
        <div class="actions">
          {#if userPaused}
            <button class="btn primary" disabled={busy} onclick={() => control('resume')}>Resume</button>
          {:else}
            <button class="btn" disabled={busy} onclick={() => control('pause')}>Pause</button>
          {/if}
        </div>
      </div>
    {:else}
      <div class="actions">
        <button class="btn" disabled={busy} onclick={() => rescan(false)}>Rescan now</button>
        <button class="btn" disabled={busy} onclick={() => { dialogError = ''; dialog = true; }}>Re-index everything…</button>
      </div>
    {/if}

    <div class="info">
      {#if live.indexer.backing_up}<div class="muted small">Backing up the catalog…</div>{/if}
      <div class="muted small">
        {#if status.next_scan}Next scan {formatWhen(status.next_scan)} · catalog{:else}Catalog{/if} backups in <span class="mono">{stats.backup_dir}</span>.
        <span class:bad={status.last_backup?.error}>{backupLine(status.last_backup)}</span>
      </div>
      <div class="muted small">Not indexed inside session folders: {stats.skip_rules.join(', ') || 'nothing extra'}, plus hidden and NAS metadata folders.</div>
    </div>

    {#if status.pending_full && !running}
      <div class="note warn">A re-index was interrupted. It carries on at the next scan (tonight, or press Rescan now).</div>
    {:else if last?.stopped}
      <div class="note warn">The last scan didn't finish. It carries on at the next nightly scan, or now with Rescan now.</div>
    {/if}

    {#if problems.length}
      <details>
        <summary>{plural(problems.length + (last.errors_omitted ?? 0), 'problem', 'problems')} in the last scan</summary>
        <ul>
          {#each problems.slice(0, MAX_ERRORS) as p, i (i)}<li class="mono">{p}</li>{/each}
          {#if last.errors_omitted}<li>…and {formatCount(last.errors_omitted)} more</li>{/if}
        </ul>
      </details>
    {/if}
  </div>

  {#if stats.missing > 0}
    <div class="card">
      <h2>⚠ {plural(stats.missing, 'file is', 'files are')} missing from the archive</h2>
      <p class="muted small lead">SD2NAS indexed these files, but they're no longer in the archive. Cards that still hold them won't be reported as safe to format. If you moved them, the next scan finds them. Ignore the ones you deleted on purpose.</p>
      <ul class="plain">
        {#each missingFiles as f (f.sha256)}
          <li class="file">
            <span class="name"><span class="mono">{f.rel_path}</span> <span class="muted small size">{formatBytes(f.size)}</span></span>
            <button class="btn link" disabled={ignoring !== ''} onclick={() => setIgnored(f, true)}>{ignoring === f.sha256 ? 'Ignoring…' : 'Ignore'}</button>
          </li>
        {/each}
        {#if stats.missing > missingFiles.length}<li class="muted">…and {formatCount(stats.missing - missingFiles.length)} more</li>{/if}
      </ul>
    </div>
  {/if}

  {#if stats.ignored > 0}
    <div class="card">
      <details class="ignored">
        <summary>{plural(stats.ignored, 'ignored missing file', 'ignored missing files')}</summary>
        <p class="muted small ignored-lead">Deleted on purpose, so they count as culled: cards that still hold them are safe to format and don't import them again. If one comes back to the archive, the next scan indexes it.</p>
        <ul class="plain">
          {#each ignoredFiles as f (f.sha256)}
            <li class="file">
              <span class="name"><span class="mono">{f.rel_path}</span> <span class="muted small size">{formatBytes(f.size)}</span></span>
              <button class="btn link" disabled={ignoring !== ''} onclick={() => setIgnored(f, false)}>{ignoring === f.sha256 ? 'Saving…' : 'Un-ignore'}</button>
            </li>
          {/each}
          {#if stats.ignored > ignoredFiles.length}<li class="muted">…and {formatCount(stats.ignored - ignoredFiles.length)} more</li>{/if}
        </ul>
      </details>
    </div>
  {/if}

  {#if stats.changed > 0 && changes}
    <div class="card">
      <h2>⚠ {changedHeading(stats.changed)}</h2>
      <p class="muted small lead">Same path, different content. Usually an editor writing metadata into the file.</p>
      <table class="stack">
        <thead><tr><th>File</th><th>Last indexed</th><th>Size change</th></tr></thead>
        <tbody>
          {#each changeList as c (c.id)}
            <tr>
              <td class="mono" data-label="File">{c.rel_path}</td>
              <td data-label="Last indexed">{datePart(c.indexed_at)}</td>
              <td class="muted" data-label="Size change">{sizeChange(c.old_size, c.size)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
      {#if changes.total > changeList.length}<p class="muted">…and {formatCount(changes.total - changeList.length)} more</p>{/if}
      {#if acceptError}<div class="note bad" role="alert">{acceptError}</div>{/if}
      <div class="row accept">
        <button class="btn primary" disabled={accepting || changes.latest_id < 1} onclick={accept}>{accepting ? 'Saving…' : 'Accept new hashes'}</button>
        <span class="muted small">The old hash is kept as an alias, so the original file from a card still counts as a duplicate.</span>
      </div>
    </div>
  {/if}

  {#if stats.recent.length}
    <div class="card">
      <h2>Recently promoted</h2>
      <table class="stack">
        <thead><tr><th>Folder</th><th class="num">Files</th><th>Promoted</th></tr></thead>
        <tbody>
          {#each stats.recent as r (r.folder)}
            <tr>
              <td data-label="Folder">{bareFolder(r.folder)}<div class="mono muted only-desktop">{r.folder}</div></td>
              <td class="num" data-label="Files">{formatCount(r.files)}</td>
              <td data-label="Promoted">{datePart(r.at)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
{/if}

<Dialog open={dialog} title="Re-index everything?" onclose={() => !busy && (dialog = false)} {busy}>
  <p class="muted">SD2NAS reads and re-hashes every file in the archive again: {formatCount(stats?.files)} files, {formatBytes(stats?.bytes)}. At array speed that takes hours. The index stays in use the whole time, so cards keep skipping files you already have; imports and promotes still go first, and you can pause it, or stop SD2NAS and it carries on later.</p>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={() => (dialog = false)} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={() => rescan(true)} disabled={busy}>Re-index everything</button>
  {/snippet}
</Dialog>

<style>
  .stats { grid-template-columns: repeat(4, minmax(0, 1fr)); }
  .stat { min-width: 0; }
  .stat b.when { font-size: 15px; padding: 5px 0; overflow-wrap: anywhere; }
  .progress { margin-top: 14px; }
  .line { overflow-wrap: anywhere; }
  .path { margin-top: 4px; }
  .actions { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 14px; }
  .info { margin-top: 14px; display: grid; gap: 4px; }
  .info .bad { color: var(--bad); }
  .bar > i.indeterminate { width: 35%; animation: slide 1.2s ease-in-out infinite alternate; transition: none; }
  @keyframes slide { from { margin-left: 0; } to { margin-left: 65%; } }
  @media (prefers-reduced-motion: reduce) { .bar > i.indeterminate { animation: none; } }
  .lead { margin: -6px 0 10px; }
  ul.plain { list-style: none; margin: 0; padding: 0; }
  ul.plain li { padding: 6px 0; border-bottom: 1px solid var(--line); overflow-wrap: anywhere; }
  ul.plain li:last-child { border-bottom: 0; }
  .size { white-space: nowrap; margin-left: 6px; }
  ul.plain li.file { display: flex; align-items: center; gap: 10px; padding: 0; }
  li.file .name { flex: 1; min-width: 0; padding: 6px 0; }
  li.file .btn { flex: none; }
  details.ignored { background: none; padding: 0; margin: 0; }
  .ignored-lead { margin: 4px 0 10px; }
  .accept { margin-top: 12px; }
  table.stack td { overflow-wrap: anywhere; }
  @media (max-width: 760px) {
    .stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .actions { flex-direction: column; }
    .actions .btn, .accept .btn { width: 100%; justify-content: center; }
    .accept { flex-direction: column; align-items: stretch; }
    table.stack, table.stack tbody, table.stack tr, table.stack td { display: block; width: 100%; }
    table.stack thead { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); }
    table.stack tr { padding: 10px 0; border-bottom: 1px solid var(--line); }
    table.stack tr:last-child { border-bottom: 0; }
    table.stack td { border: 0; padding: 2px 0; display: flex; gap: 10px; justify-content: space-between; text-align: left; }
    table.stack td.num { text-align: left; }
    table.stack td::before { content: attr(data-label); color: var(--muted); font-size: 12px; flex: none; }
    table.stack td:first-child { display: block; font-weight: 500; }
    table.stack td:first-child::before { display: block; }
  }
</style>
