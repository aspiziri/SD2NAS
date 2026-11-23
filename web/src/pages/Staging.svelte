<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { formatBytes, formatCount, dateRange } from '../lib/format.js';
  import { clickAction, toggleKey, selectRange, prune } from '../lib/selection.js';
  import { splitName, folderName, folderDate, earliestDate, checkTitle, promoteTarget, DEFAULT_PATTERN } from '../lib/naming.js';
  import { describeError, rescanSummary, plural, promoteStates } from '../lib/messages.js';
  import { longPressSelect } from '../lib/press.js';
  import { toast } from '../lib/toast.svelte.js';
  import Thumb from '../components/Thumb.svelte';
  import Dialog from '../components/Dialog.svelte';
  import SelectionBar from '../components/SelectionBar.svelte';
  import PromoteProgress from '../components/PromoteProgress.svelte';

  const PATTERN_KEY = 'sd2nas.promotePattern';
  const RECENT_MS = 10 * 60_000; // how long a finished promote stays on screen

  let data = $state(null);
  let error = $state('');
  let ctl = null; // the load in flight; a newer one aborts it

  let selected = $state(new Set()); // folder ids as strings
  let anchor = null; // the last clicked id, for Shift-click ranges

  let dialog = $state(''); // '' | 'rename' | 'merge' | 'discard' | 'promote'
  let busy = $state(false);
  let dialogError = $state('');
  let title = $state('');
  let pattern = $state(DEFAULT_PATTERN);
  let dismissedJob = $state(0); // the last finished job the user dismissed
  let cancelling = $state(0); // the queued job being cancelled
  let rescanAsked = false;
  let rescanProblemsSeen = $state(null); // finished_at of the rescan whose problems were dismissed
  let rescanSeen = null; // finished_at of the last rescan already handled

  async function load() {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api('/api/staging', { signal: mine.signal });
      if (mine.signal.aborted) return;
      data = res;
      error = '';
      selected = prune(selected, res.folders.map((f) => String(f.id)));
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      error = err.message;
    }
  }

  // Load on mount, when an import finishes, when the stream comes back after
  // a real drop, and when staging.changed carries a version other than the
  // one on screen (Decision 14).
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    const version = live.staging.version;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      const stale = data !== null && version !== data.version;
      if (!started || reconnected || finished || stale) {
        started = true;
        load();
      }
    });
  });
  $effect(() => () => ctl?.abort());

  // Report a rescan this page asked for (the startup one stays quiet).
  $effect(() => {
    const r = live.staging.last_rescan;
    untrack(() => {
      if (!r || r.finished_at === rescanSeen) return;
      rescanSeen = r.finished_at;
      if (!rescanAsked) return;
      rescanAsked = false;
      toast(rescanSummary(r) + (r.errors_omitted ? ` (and ${plural(r.errors_omitted, 'more problem', 'more problems')} not listed)` : ''), r.skipped || r.errors?.length ? 'warn' : 'ok', 6000);
    });
  });

  // Folders a promote holds (queued or moving) can't be renamed, merged or
  // discarded (the server would answer 409), so they aren't selectable.
  const queue = $derived(live.promote);
  const held = $derived(promoteStates(queue));
  const folders = $derived(data?.folders ?? []);
  const order = $derived(folders.filter((f) => !held.has(f.id)).map((f) => String(f.id)));
  const chosen = $derived(folders.filter((f) => selected.has(String(f.id))));
  const totals = $derived({ files: folders.reduce((s, f) => s + f.files, 0), bytes: folders.reduce((s, f) => s + f.bytes, 0) });
  const showLast = $derived(!!queue?.last && queue.last.id !== dismissedJob && Date.now() - Date.parse(queue.last.finished_at) < RECENT_MS);
  const rescanProblems = $derived.by(() => {
    const r = live.staging.last_rescan;
    return r?.errors?.length && r.finished_at !== rescanProblemsSeen ? r : null;
  });
  const showQueue = $derived(!!queue && (queue.running || queue.queued.length > 0 || showLast));
  // A held folder leaves the selection as soon as its promote is queued.
  $effect(() => {
    const ids = order;
    untrack(() => (selected = prune(selected, ids)));
  });

  function toggle(key) {
    if (!order.includes(key)) return; // held by a promote
    selected = toggleKey(selected, key);
    anchor = key;
  }
  function clear() {
    selected = new Set();
    anchor = null;
  }
  function selectAll() {
    selected = new Set(order);
  }

  // Desktop: click opens, Ctrl/⌘-click toggles, Shift-click ranges; while
  // anything is selected a click toggles. Touch: tap opens, long-press
  // selects, then taps toggle (Decision 18).
  function onFolderClick(e, f) {
    if (e.target.closest('.check')) return; // the checkbox handles itself
    const key = String(f.id);
    const act = held.has(f.id) ? 'open' : clickAction({ selecting: selected.size > 0, shift: e.shiftKey, toggle: e.ctrlKey || e.metaKey });
    if (act === 'open') {
      if (selected.size && held.has(f.id)) return; // selecting: a held folder does nothing
      if (!e.target.closest('a')) navigate(`/staging/${f.id}`); // a click on the name is a link click already
      return;
    }
    e.preventDefault(); // keeps App's link handler from opening the folder
    selected = act === 'range' ? selectRange(selected, order, anchor, key) : toggleKey(selected, key);
    anchor = key;
  }
  const press = { onlong: (key) => { if (!selected.has(key)) toggle(key); } };

  function onkeydown(e) {
    if (dialog || e.target.closest?.('input, textarea, select')) return;
    if (e.key === 'Escape' && selected.size) clear();
    else if ((e.ctrlKey || e.metaKey) && e.key === 'a' && folders.length) {
      e.preventDefault();
      selectAll();
    }
  }

  function loadPattern() {
    try {
      return localStorage.getItem(PATTERN_KEY) || DEFAULT_PATTERN;
    } catch {
      return DEFAULT_PATTERN; // storage unavailable (private mode): use the default
    }
  }
  function savePattern(p) {
    try {
      localStorage.setItem(PATTERN_KEY, p);
    } catch {
      // storage unavailable (private mode): the next promote starts from the default again
    }
  }

  function openDialog(kind) {
    dialogError = '';
    if (kind === 'rename') {
      const n = splitName(chosen[0].name);
      title = n.date ? n.title : chosen[0].name;
    } else if (kind === 'merge') {
      title = mergeSorted.map((f) => (splitName(f.name).date ? splitName(f.name).title : f.name)).find(Boolean) ?? '';
    } else if (kind === 'promote') {
      pattern = loadPattern();
    }
    dialog = kind;
  }
  function closeDialog() {
    if (!busy) dialog = '';
  }

  // run sends one action; on success the dialog closes, on failure it stays
  // open with the reason (409s name the busy folder).
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

  // The whole "date - title" name is checked against the byte limit.
  const titleCheck = $derived(checkTitle(title, dialog === 'merge' ? earliestDate(chosen) : dialog === 'rename' && chosen[0] ? folderDate(chosen[0]) : ''));

  // Rename
  const renaming = $derived(chosen.length === 1 ? chosen[0] : null);
  const renameName = $derived(renaming ? folderName(folderDate(renaming), titleCheck.title) : '');
  function doRename() {
    const f = renaming;
    run(async () => {
      const res = await api(`/api/staging/${f.id}`, { method: 'PATCH', body: { title: titleCheck.title } });
      toast(`Renamed to “${res.folder.name}”`);
      clear();
    });
  }

  // Merge
  const mergeSorted = $derived([...chosen].sort((a, b) => (folderDate(a) || '9999').localeCompare(folderDate(b) || '9999') || a.id - b.id));
  const mergeName = $derived(folderName(earliestDate(chosen), titleCheck.title));
  const mergeFiles = $derived(chosen.reduce((s, f) => s + f.files, 0));
  function doMerge() {
    run(async () => {
      const res = await api('/api/staging/merge', { method: 'POST', body: { ids: chosen.map((f) => f.id), title: titleCheck.title } });
      toast(`Merged into “${res.folder.name}”`);
      selected = new Set([String(res.folder.id)]);
      anchor = null;
    });
  }

  // Discard (one request per folder; stops at the first failure)
  function doDiscard() {
    const list = [...chosen];
    run(async () => {
      let files = 0;
      let removed = 0;
      const kept = [];
      try {
        for (const f of list) {
          const res = await api(`/api/staging/${f.id}`, { method: 'DELETE' });
          files += res.culled;
          if (res.removed) removed++;
          else kept.push(`“${f.name}” (${res.kept.join(', ')})`);
        }
      } finally {
        if (removed || files) toast(`Discarded ${plural(removed, 'folder', 'folders')} (${plural(files, 'file', 'files')})`);
        if (kept.length) toast(`Kept ${kept.join('; ')}: SD2NAS doesn't track those files. Rescan, then discard again.`, 'warn', 9000);
      }
      clear();
    });
  }

  // Promote
  const targets = $derived(chosen.map((f) => ({ f, ...promoteTarget(f, pattern) })));
  const patternError = $derived(targets.find((t) => t.error)?.error ?? '');
  const undated = $derived(chosen.filter((f) => !splitName(f.name).title || !splitName(f.name).date));
  const promoteBytes = $derived(chosen.reduce((s, f) => s + f.bytes, 0));
  function doPromote() {
    const p = pattern.trim() || DEFAULT_PATTERN;
    run(async () => {
      const res = await api('/api/staging/promote', { method: 'POST', body: { ids: chosen.map((f) => f.id), pattern: p } });
      savePattern(p);
      clear();
      if (res.job.state === 'queued') toast(`Queued: ${plural(res.job.folders.length, 'folder waits', 'folders wait')} for the promote ahead of ${res.job.folders.length === 1 ? 'it' : 'them'}.`);
      // Show the queue now; promote.progress events take over.
      live.promote = await api('/api/staging/promote');
    });
  }

  // Cancel a queued promote: its folders stay in staging and can be changed again.
  async function cancelPromote(job) {
    cancelling = job.id;
    try {
      const res = await api(`/api/staging/promote/${job.id}`, { method: 'DELETE' });
      toast(`Cancelled the promote of ${res.job.folders.map((f) => `“${f.name}”`).join(', ')}.`);
      live.promote = await api('/api/staging/promote');
    } catch (err) {
      toast(describeError(err), 'bad');
    } finally {
      cancelling = 0;
    }
  }

  async function rescan() {
    rescanAsked = true;
    try {
      const res = await api('/api/staging/rescan', { method: 'POST' });
      if (!res.started) toast('A rescan is already running.', 'warn');
    } catch (err) {
      rescanAsked = false;
      toast(describeError(err), 'bad');
    }
  }
</script>

<svelte:window {onkeydown} />

<div class="row head">
  <h1>Staging</h1>
  <span class="spacer"></span>
  <button class="btn" onclick={rescan} disabled={live.staging.rescanning}>{live.staging.rescanning ? 'Rescanning…' : 'Rescan staging'}</button>
</div>
{#if error && data}
  <div class="note bad" role="alert">Couldn't refresh staging: {error}</div>
{/if}
{#if error && !data}
  <div class="note bad">Couldn't load staging: {error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  <p class="sub">{plural(folders.length, 'folder', 'folders')} · {formatCount(totals.files)} files · {formatBytes(totals.bytes)}</p>
  {#if showQueue}
    <PromoteProgress {queue} {showLast} {cancelling} oncancel={cancelPromote} ondismiss={(j) => (dismissedJob = j.id)} />
  {/if}
  {#if rescanProblems}
    <div class="note warn" role="status">
      <div class="row">
        <b>The last rescan had {plural(rescanProblems.errors.length + (rescanProblems.errors_omitted ?? 0), 'problem', 'problems')}:</b>
        <span class="spacer"></span>
        <button class="btn link" onclick={() => (rescanProblemsSeen = rescanProblems.finished_at)}>Dismiss</button>
      </div>
      <ul class="problems">
        {#each rescanProblems.errors as e}<li>{e}</li>{/each}
        {#if rescanProblems.errors_omitted}<li>and {formatCount(rescanProblems.errors_omitted)} more</li>{/if}
      </ul>
    </div>
  {/if}
  {#if data.stale}
    <div class="note warn">{plural(data.stale, 'folder has', 'folders have')} been in staging longer than {data.stale_after_days} days. Promote or discard {data.stale === 1 ? 'it' : 'them'}.</div>
  {/if}
  {#if data.untracked.length}
    <div class="note warn row">
      <span>Found {plural(data.untracked.length, 'folder', 'folders')} SD2NAS doesn't track yet: {data.untracked.join(', ')}.</span>
      <button class="btn" onclick={rescan} disabled={live.staging.rescanning}>Rescan to add {data.untracked.length === 1 ? 'it' : 'them'}</button>
    </div>
  {/if}
  {#if folders.length === 0}
    <div class="waiting">
      <h2>Staging is empty</h2>
      <p class="muted" style="margin:0">New imports appear here.</p>
    </div>
  {:else}
    {#if selected.size}
      <SelectionBar count={selected.size} total={folders.length} onclear={clear} onall={selectAll}>
        <button class="btn" disabled={selected.size < 2} onclick={() => openDialog('merge')}>Merge</button>
        <button class="btn" disabled={selected.size !== 1} onclick={() => openDialog('rename')}>Rename</button>
        <button class="btn danger" onclick={() => openDialog('discard')}>Discard</button>
        <button class="btn primary" onclick={() => openDialog('promote')}>Promote →</button>
      </SelectionBar>
    {:else}
      <div class="toolbar row">
        <span class="muted only-desktop">Select folders to merge, rename, discard or promote them.</span>
        <span class="muted only-phone">Long-press a folder to select it.</span>
        <span class="spacer"></span>
        <button class="btn link only-desktop" onclick={selectAll}>Select all</button>
      </div>
    {/if}
    <div class="folders selectable" use:longPressSelect={press}>
      {#each folders as f (f.id)}
        {@const key = String(f.id)}
        <!-- The name link is the keyboard path; a click anywhere on the card is a mouse/touch shortcut. -->
        {@const hold = held.get(f.id)}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <div class="folder" class:sel={selected.has(key)} class:held={!!hold} data-key={key} onclick={(e) => onFolderClick(e, f)}>
          <div class="top">
            <label class="check" title={hold ? 'Waiting for its promote' : 'Select'}>
              <input type="checkbox" checked={selected.has(key)} disabled={!!hold} onchange={() => toggle(key)} aria-label="Select {f.name}" />
            </label>
            <div class="title">
              <a class="name" href="/staging/{f.id}">{f.name}</a>
              <div class="muted small">{dateRange(f.dates)}</div>
            </div>
            {#if f.stale}<span class="pill warn" title="In staging for {f.age_days} days">{f.age_days} days</span>{/if}
          </div>
          {#if hold}
            <div class="row small hold">
              <span class="pill live">{hold.state === 'moving' ? 'Promoting…' : 'Waiting to promote'}</span>
              {#if hold.cancellable}
                <button class="btn" disabled={cancelling === hold.job} onclick={(e) => { e.stopPropagation(); cancelPromote(queue.queued.find((j) => j.id === hold.job)); }}>Cancel</button>
              {/if}
            </div>
          {/if}
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
            <span class="muted">{plural(f.files, 'file', 'files')} · {formatBytes(f.bytes)}</span>
          </div>
          {#each f.warnings as w}<div class="warn small">⚠ {w}</div>{/each}
        </div>
      {/each}
    </div>
  {/if}
{/if}

<Dialog open={dialog === 'rename'} title="Rename folder" onclose={closeDialog} {busy}>
  {#if renaming}
    <label class="f" for="rename-title">Folder title</label>
    <div class="prefixed">
      {#if folderDate(renaming)}<span class="mono">{folderDate(renaming)} -</span>{/if}
      <!-- svelte-ignore a11y_autofocus -->
      <input id="rename-title" type="text" bind:value={title} placeholder="e.g. Yosemite" autofocus
        onkeydown={(e) => e.key === 'Enter' && !busy && !titleCheck.error && renameName && renameName !== renaming.name && doRename()} />
    </div>
    {#if titleCheck.error}<div class="field-error">{titleCheck.error}</div>{/if}
    <p class="muted small">The date stays at the front so the archive stays sorted, e.g. <span class="mono">2007-11-10 - SMU Homecoming</span>.</p>
    {#if renameName}<p class="small">New name: <span class="mono">{renameName}</span></p>{/if}
  {/if}
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doRename} disabled={busy || !!titleCheck.error || !renameName || renameName === renaming?.name}>{busy ? 'Renaming…' : 'Rename'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'merge'} title="Merge {chosen.length} folders" onclose={closeDialog} {busy}>
  <ul class="muted list">
    {#each mergeSorted as f (f.id)}<li>{f.name} <span class="small">({plural(f.files, 'file', 'files')})</span></li>{/each}
  </ul>
  <label class="f" for="merge-title">Folder title</label>
  <div class="prefixed">
    {#if earliestDate(chosen)}<span class="mono">{earliestDate(chosen)} -</span>{/if}
    <input id="merge-title" type="text" bind:value={title} placeholder="e.g. Yosemite" />
  </div>
  {#if titleCheck.error}<div class="field-error">{titleCheck.error}</div>{/if}
  {#if mergeName}<p class="small">Merged folder: <span class="mono">{mergeName}</span></p>{/if}
  <div class="note">{plural(mergeFiles, 'file ends', 'files end')} up in one folder, still sorted into <span class="mono">Capture/</span> and <span class="mono">Videos/</span>. Filenames keep their timestamps; if two names clash, the second gets a -1 suffix.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doMerge} disabled={busy || !!titleCheck.error || !mergeName}>{busy ? 'Merging…' : 'Merge'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'discard'} title="Discard {plural(chosen.length, 'folder', 'folders')}?" onclose={closeDialog} {busy} danger>
  <p class="muted">{plural(mergeFiles, 'file is', 'files are')} deleted from staging: {chosen.map((f) => f.name).join(', ')}.</p>
  <div class="note warn">These files stay in the ingest history and won't be re-imported from the card. To get them back, use “Re-import anyway” on that import's page.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn bad" onclick={doDiscard} disabled={busy}>{busy ? 'Discarding…' : `Discard ${plural(mergeFiles, 'file', 'files')}`}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'promote'} title="Promote {plural(chosen.length, 'folder', 'folders')} to the archive" onclose={closeDialog} {busy}>
  <label class="f" for="promote-pattern">Destination pattern</label>
  <input id="promote-pattern" type="text" bind:value={pattern} spellcheck="false" autocapitalize="off" />
  <p class="muted small" style="margin:4px 0 0">Tokens: <span class="mono">{'{year}'}</span> <span class="mono">{'{month}'}</span> <span class="mono">{'{name}'}</span></p>
  <div class="preview mono">
    {#each targets as t (t.f.id)}<div>{t.error ? `${t.f.name}: ${t.error}` : `Archive/${t.path}/`}</div>{/each}
  </div>
  {#if undated.length}
    <div class="note warn">{plural(undated.length, 'folder has', 'folders have')} only a date or only a title for a name. Rename first if you want “date - title” in the archive.</div>
  {/if}
  <div class="note">{plural(mergeFiles, 'file', 'files')} ({formatBytes(promoteBytes)}) move into the archive. Within the photos share that is an instant rename; a folder that already exists in the archive is merged file by file.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doPromote} disabled={busy || !!patternError}>{busy ? 'Queueing…' : queue?.running || queue?.queued.length ? 'Add to the queue' : 'Promote'}</button>
  {/snippet}
</Dialog>

<style>
  .head { margin-bottom: 4px; }
  .head h1 { margin: 0; }
  .folders { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 14px; }
  .folder { display: flex; flex-direction: column; gap: 4px; background: var(--panel); border: 1px solid var(--line); border-radius: var(--r);
    padding: 8px 12px 12px; color: var(--text); min-width: 0; cursor: pointer; transition: border-color .1s; }
  .folder:hover { border-color: #41567a; }
  .folder.sel { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
  .folder.held { opacity: .75; }
  .hold { margin: 2px 0 4px; }
  .top { display: flex; gap: 4px; align-items: flex-start; }
  .check { display: grid; place-items: center; width: 44px; height: 44px; margin: -2px 0 0 -10px; flex: none; cursor: pointer; }
  .title { flex: 1; min-width: 0; padding-top: 10px; }
  .name { font-weight: 600; color: var(--text); text-decoration: none; overflow-wrap: anywhere; }
  .name:hover { color: var(--accent); text-decoration: underline; }
  .top .pill { margin-top: 12px; }
  .strip { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 4px; margin: 6px 0; }
  .warn { color: var(--warn); }
  .list { margin: 0; padding-left: 18px; }
  .problems { margin: 6px 0 0; padding-left: 18px; overflow-wrap: anywhere; }
  @media (max-width: 760px) {
    .folders { grid-template-columns: minmax(0, 1fr); }
  }
</style>
