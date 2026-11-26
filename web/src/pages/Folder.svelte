<script>
  import { onMount, untrack, tick } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { isId } from '../lib/paths.js';
  import { formatBytes, dateRange, kindChips, extLabel, formatCapture } from '../lib/format.js';
  import { needsReload } from '../lib/views.js';
  import { loadPref, savePref, FOLDER_VIEW_KEY } from '../lib/prefs.js';
  import { clickAction, toggleKey, selectRange, dragSelection, prune } from '../lib/selection.js';
  import { folderDate, folderName, checkFolderName } from '../lib/naming.js';
  import { describeError, plural } from '../lib/messages.js';
  import { longPressSelect } from '../lib/press.js';
  import { toast } from '../lib/toast.svelte.js';
  import Thumb from '../components/Thumb.svelte';
  import Lightbox from '../components/Lightbox.svelte';
  import Dialog from '../components/Dialog.svelte';
  import SelectionBar from '../components/SelectionBar.svelte';
  import ViewSwitch from '../components/ViewSwitch.svelte';

  let { id } = $props();
  let kind = $state(''); // the chip asked for; '' = all
  let data = $state(null);
  let shownKind = $state(''); // the kind `data` was loaded for
  let error = $state('');
  let gone = $state(false); // the folder no longer exists (merged, promoted, discarded)
  let open = $state(-1); // index into data.files shown full screen; -1 = none
  let ctl = null;
  let reloadedOnce = false; // F10: one extra load when a load returns a version other than the live one

  const VIEWS = [{ value: 'grid', label: 'Grid' }, { value: 'list', label: 'List' }];
  let view = $state(loadPref(FOLDER_VIEW_KEY, VIEWS.map((v) => v.value), 'grid'));
  function setView(v) {
    view = v;
    savePref(FOLDER_VIEW_KEY, v);
  }

  let selected = $state(new Set()); // file hashes
  let anchor = null;
  let drag = null; // { base, anchor, adding } while a finger drag-selects

  let dialog = $state(''); // '' | 'move' | 'cull'
  let busy = $state(false);
  let dialogError = $state('');
  let others = $state(null); // the other staging folders, for "Move to…"
  let dest = $state('new'); // 'new' or a folder id as a string
  let nameRaw = $state(''); // the new folder's whole name, as typed
  let nameInput = $state(null);

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
      const sha = open >= 0 ? data?.files[open]?.sha256 : null;
      data = res;
      shownKind = k;
      error = '';
      selected = prune(selected, res.files.map((f) => f.sha256));
      if (open >= 0) {
        const j = res.files.findIndex((x) => x.sha256 === sha);
        if (j >= 0) open = j;
        else hide();
      }
      // The event that announced this version may have arrived while the
      // request ran: if the stream is ahead of what we got, load once more.
      if (needsReload(res.version, live.staging.version, live.connected)) {
        if (!reloadedOnce) {
          reloadedOnce = true;
          load(k);
        }
      } else reloadedOnce = false;
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      if (err.status === 404) {
        gone = true;
        data = null;
      }
      error = err.message;
    }
  }

  // M3's triggers, plus staging.changed with a version other than the one
  // on screen (another tab or device changed this folder).
  let started = false;
  let lastKind = '';
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  let scanSeen; // finished_at of the last archive scan; a new one may mark files as in the archive
  $effect(() => {
    const k = kind;
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    const version = live.staging.version;
    const scan = live.indexer.last?.finished_at ?? null;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen || (scanSeen !== undefined && scan !== scanSeen);
      finishedSeen = finishedCount;
      scanSeen = scan;
      const changed = k !== lastKind;
      lastKind = k;
      const stale = data !== null && version !== data.version;
      if (!gone && (!started || changed || reconnected || finished || stale)) {
        started = true;
        load(k);
      }
    });
  });
  $effect(() => () => ctl?.abort());

  onMount(() => {
    if (history.state?.lightbox) history.back();
  });

  function show(i) {
    if (open >= 0) return;
    open = i;
    history.pushState({ lightbox: true }, '');
  }
  function hide() {
    if (history.state?.lightbox) history.back();
    else open = -1;
  }
  function onpopstate() {
    if (!history.state?.lightbox) open = -1;
  }

  const files = $derived(data?.files ?? []);
  const order = $derived(files.map((f) => f.sha256));
  const selecting = $derived(selected.size > 0);
  const inArchive = $derived(files.filter((f) => f.in_archive).map((f) => f.sha256));
  const selectedInArchive = $derived(inArchive.filter((sha) => selected.has(sha)).length);

  function toggle(key) {
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
  function selectInArchive() {
    selected = new Set(inArchive);
    anchor = null;
  }

  function onTileClick(e, i) {
    const key = files[i].sha256;
    const act = clickAction({ selecting, shift: e.shiftKey, toggle: e.ctrlKey || e.metaKey });
    if (act === 'open') return show(i);
    selected = act === 'range' ? selectRange(selected, order, anchor, key) : toggleKey(selected, key);
    anchor = key;
  }

  // Touch: long-press selects (or unselects) a tile; keep the finger down
  // and drag to do the same to every tile between it and the finger.
  const press = {
    onlong(key) {
      const adding = !selected.has(key);
      drag = { base: new Set(selected), anchor: key, adding };
      selected = dragSelection(drag.base, order, key, key, adding);
    },
    ondrag(key) {
      if (drag) selected = dragSelection(drag.base, order, drag.anchor, key, drag.adding);
    },
    ondragend() {
      if (drag) anchor = drag.anchor;
      drag = null;
    },
  };

  function onkeydown(e) {
    if (dialog || open >= 0 || e.target.closest?.('input, textarea, select')) return;
    if (e.key === 'Escape' && selecting) clear();
    else if ((e.ctrlKey || e.metaKey) && e.key === 'a' && files.length) {
      e.preventDefault();
      selectAll();
    }
  }

  // run sends one action. fn returns true when it navigated away.
  async function run(fn) {
    busy = true;
    dialogError = '';
    try {
      const left = await fn();
      dialog = '';
      if (!left) load(shownKind);
    } catch (err) {
      dialogError = describeError(err);
      load(shownKind);
    } finally {
      busy = false;
    }
  }

  const prefix = $derived(data ? folderDate(data.folder) : '');
  const nameCheck = $derived(checkFolderName(nameRaw));
  // The prefill alone ("date - ") would make a date-only folder by accident,
  // so Move waits for an edit. A new folder can't take this folder's own
  // name either (the server would say it's taken).
  const untouched = $derived(nameRaw.trim() === (prefix ? `${prefix} -` : ''));
  const moveReady = $derived(dest === 'new' ? !untouched && !!nameCheck.name && !nameCheck.error && nameCheck.name !== data?.folder.name : dest !== '');

  // The new folder's name starts as "date - " with the caret at the end.
  async function focusName() {
    await tick();
    if (dialog !== 'move' || dest !== 'new' || !nameInput) return;
    nameInput.focus();
    nameInput.setSelectionRange(nameRaw.length, nameRaw.length);
  }

  async function openMove() {
    dialogError = '';
    nameRaw = prefix ? `${prefix} - ` : '';
    dest = 'new';
    others = null;
    dialog = 'move';
    focusName();
    try {
      const res = await api('/api/staging');
      others = res.folders.filter((f) => f.id !== data.folder.id);
    } catch (err) {
      others = [];
      dialogError = `Couldn't list the other folders: ${describeError(err)}`;
    }
  }

  function doMove() {
    const shas = [...selected];
    const fid = data.folder.id; // a refetch may clear data while the request runs (pre-flight D13)
    const body = dest === 'new' ? { fileShas: shas, newName: nameCheck.name } : { fileShas: shas, toId: Number(dest) };
    run(async () => {
      const res = await api('/api/staging/move', { method: 'POST', body });
      toast(`Moved ${plural(res.moved, 'file', 'files')} to “${res.folder.name}”`);
      clear();
      if (res.removed.includes(fid)) {
        navigate('/staging', { replace: true });
        return true;
      }
      return false;
    });
  }

  function doCull() {
    const shas = [...selected];
    const fid = data.folder.id;
    run(async () => {
      const res = await api('/api/staging/cull', { method: 'POST', body: { fileShas: shas } });
      toast(`Culled ${plural(res.culled, 'file', 'files')}`);
      clear();
      if (res.removed.includes(fid)) {
        navigate('/staging', { replace: true });
        return true;
      }
      return false;
    });
  }
</script>

<svelte:window {onpopstate} {onkeydown} />

<p><a href="/staging" class="btn">← Staging</a></p>

{#if gone}
  <div class="note warn">This folder no longer exists: it was merged, promoted or discarded. <a href="/staging">Back to staging</a></div>
{:else if error && !data}
  <div class="note bad">{error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  {@const f = data.folder}
  <h1>{f.name}</h1>
  <p class="sub">{dateRange(f.dates)} · {plural(f.files, 'file', 'files')} · {formatBytes(f.bytes)}</p>
  {#if error}
    <div class="note bad" role="alert">Couldn't refresh this folder: {error}</div>
  {/if}
  {#each f.warnings as w}<div class="note warn">{w}</div>{/each}
  {#if selecting}
    <SelectionBar count={selected.size} total={files.length} onclear={clear} onall={selectAll}>
      <button class="btn" onclick={openMove}>Move to…</button>
      <button class="btn danger" onclick={() => { dialogError = ''; dialog = 'cull'; }}>Cull</button>
    </SelectionBar>
  {/if}
  <div class="toolbar row" class:hidden-phone={selecting}>
    <div class="chips">
      {#each kindChips(f.counts) as c (c.kind)}
        <button class="chip" class:on={shownKind === c.kind} onclick={() => (kind = c.kind)}>{c.label} {c.count}</button>
      {/each}
    </div>
    <span class="spacer"></span>
    <ViewSwitch label="View" options={VIEWS} value={view} onchange={setView} />
    {#if inArchive.length && !selecting}
      <button class="btn" onclick={selectInArchive}>Select {inArchive.length} in archive</button>
    {:else if !selecting}
      <span class="muted small only-desktop">Ctrl-click or tick files to select them</span>
      <span class="muted small only-phone">Long-press to select; drag to select more</span>
    {/if}
  </div>
  {#if files.length === 0}
    <p class="muted">No files here.</p>
  {:else}
    {#if view === 'list'}
      <div class="rows selectable" class:selecting use:longPressSelect={press}>
        {#each files as file, i (file.sha256)}
          {@const on = selected.has(file.sha256)}
          <div class="lrow" class:sel={on} data-key={file.sha256}>
            <button class="lpick" onclick={() => toggle(file.sha256)} aria-pressed={on} aria-label="{on ? 'Unselect' : 'Select'} {file.name}">
              <span class="box" class:on>✓</span>
            </button>
            <button class="lbody" onclick={(e) => onTileClick(e, i)} aria-label={selecting ? `${on ? 'Unselect' : 'Select'} ${file.name}` : `Show ${file.name}`}>
              <span class="lthumb"><Thumb sha={file.sha256} kind={file.kind} label={extLabel(file)} /></span>
              <span class="linfo">
                <span class="lname">{#if file.clock_warnings?.length}<span class="warn" title={file.clock_warnings.join('; ')}>⚠ </span>{/if}{file.name}</span>
                <span class="lmeta small muted">
                  {#if file.capture_time}<span>{formatCapture(file.capture_time)}</span>{/if}
                  <span>{file.kind === 'raw' ? 'RAW' : file.kind === 'video' ? 'Video' : 'JPG'}</span>
                  <span>{formatBytes(file.size)}</span>
                  {#if file.clock_warnings?.length}<span class="warn">Check the clock</span>{/if}
                  {#if file.in_archive}<span class="pill ok" title="Already in the archive: {file.in_archive}">In archive</span>{/if}
                </span>
              </span>
            </button>
          </div>
        {/each}
      </div>
    {:else}
    <div class="grid selectable" class:selecting use:longPressSelect={press}>
      {#each files as file, i (file.sha256)}
        {@const on = selected.has(file.sha256)}
        <div class="cell" class:sel={on} data-key={file.sha256}>
          <button class="tile" onclick={(e) => onTileClick(e, i)} aria-label={selecting ? `${on ? 'Unselect' : 'Select'} ${file.name}` : `Show ${file.name}`}>
            <Thumb sha={file.sha256} kind={file.kind} label={extLabel(file)} />
            <span class="fn">{#if file.clock_warnings?.length}<span class="warn" title={file.clock_warnings.join('; ')}>⚠ </span>{/if}{file.name}</span>
            {#if file.in_archive}<span class="inarch" title="Already in the archive: {file.in_archive}">In archive</span>{/if}
          </button>
          <button class="pick" class:on onclick={() => toggle(file.sha256)} aria-pressed={on} aria-label="{on ? 'Unselect' : 'Select'} {file.name}"><span>✓</span></button>
        </div>
      {/each}
    </div>
    {/if}
  {/if}
  {#if open >= 0 && open < files.length}
    <Lightbox files={data.files} bind:index={open} onclose={hide} />
  {/if}

  <Dialog open={dialog === 'move'} title="Move {plural(selected.size, 'file', 'files')}" onclose={() => !busy && (dialog = '')} {busy}>
    <div class="dests" role="radiogroup" aria-label="Destination">
      <label class="dest"><input type="radio" bind:group={dest} value="new" /> <span>New folder…</span></label>
      {#if dest === 'new'}
        <div class="newfolder">
          <input type="text" bind:this={nameInput} bind:value={nameRaw} placeholder={folderName(prefix, 'Picks')} aria-label="New folder name"
            onkeydown={(e) => e.key === 'Enter' && !busy && moveReady && doMove()} />
          {#if nameCheck.error}<div class="field-error">{nameCheck.error}</div>{/if}
          {#if !untouched && nameCheck.name && nameCheck.name !== nameRaw.trim()}<p class="small" style="margin:6px 0 0">New folder: <span class="mono">{nameCheck.name}</span></p>{/if}
        </div>
      {/if}
      {#if others === null}
        <p class="muted small">Loading folders…</p>
      {:else}
        {#each others as o (o.id)}
          <label class="dest"><input type="radio" bind:group={dest} value={String(o.id)} /> <span>{o.name} <span class="muted small">({plural(o.files, 'file', 'files')})</span></span></label>
        {/each}
      {/if}
    </div>
    <p class="muted small">Files keep their names and their <span class="mono">Capture/</span> or <span class="mono">Videos/</span> subfolder; a clash gets a -1 suffix.</p>
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn primary" onclick={doMove} disabled={busy || !moveReady}>{busy ? 'Moving…' : 'Move'}</button>
    {/snippet}
  </Dialog>

  <Dialog open={dialog === 'cull'} title="Cull {plural(selected.size, 'file', 'files')}?" onclose={() => !busy && (dialog = '')} {busy} danger>
    <p class="muted">They are deleted from staging.</p>
    <div class="note">They stay in the ingest history, so they won't be re-imported the next time this card goes in.</div>
    {#if selectedInArchive}
      <div class="note">{selectedInArchive === selected.size ? (selected.size === 1 ? 'It is' : 'They are') : `${selectedInArchive} of them are`} already in the archive. Those archive copies stay, and the next archive scan records them as archived.</div>
    {/if}
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn bad" onclick={doCull} disabled={busy}>{busy ? 'Culling…' : `Cull ${plural(selected.size, 'file', 'files')}`}</button>
    {/snippet}
  </Dialog>
{/if}

<style>
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 8px; }
  .cell { position: relative; min-width: 0; border-radius: 6px; }
  .tile { display: block; width: 100%; background: none; border: 0; padding: 0; color: inherit; font: inherit; text-align: left; cursor: pointer; border-radius: 6px; }
  .tile:hover { outline: 2px solid var(--accent); outline-offset: 1px; }
  .cell.sel :global(.thumb) { outline: 3px solid var(--accent); outline-offset: 1px; }
  .cell.sel .tile :global(img) { opacity: .8; }
  .fn { display: block; font-size: 11px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-top: 3px; }
  .warn { color: var(--warn); }
  /* "In archive": top-right on the thumbnail (the tick is top-left); taps go through to the tile. */
  .inarch { position: absolute; top: 4px; right: 4px; pointer-events: none; font-size: 10px; font-weight: 600; line-height: 1;
    padding: 3px 6px; border-radius: 999px; background: rgba(0,0,0,.6); color: #fff; border: 1px solid var(--ok); }
  /* The tick: a 44 px hit area with a 24 px circle, top-left on the thumbnail.
     Shown on hover with a mouse, and always while selecting. */
  .pick { position: absolute; top: 0; left: 0; width: 44px; height: 44px; padding: 0; border: 0; background: none; cursor: pointer;
    display: grid; place-items: center; opacity: 0; transition: opacity .1s; }
  .pick span { width: 24px; height: 24px; border-radius: 50%; display: grid; place-items: center; font-size: 13px; font-weight: 700;
    background: rgba(0,0,0,.45); border: 2px solid #fff; color: transparent; }
  .pick.on span { background: var(--accent); border-color: var(--accent); color: #fff; }
  .selecting .pick, .pick.on, .pick:focus-visible { opacity: 1; }
  @media (hover: hover) { .cell:hover .pick { opacity: 1; } }
  /* On touch the invisible tick must not swallow taps meant for the tile. */
  @media (hover: none) { .grid:not(.selecting) .pick { pointer-events: none; } }
  /* List view: a 56 px thumbnail, then the name and details. The tick is its own 44 px target. */
  .rows { display: flex; flex-direction: column; gap: 4px; }
  .lrow { display: flex; align-items: center; min-height: 56px; min-width: 0; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; }
  .lrow.sel { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
  .lpick { flex: none; width: 44px; align-self: stretch; min-height: 56px; padding: 0; border: 0; background: none; cursor: pointer; display: grid; place-items: center; }
  .box { width: 22px; height: 22px; border-radius: 50%; display: grid; place-items: center; font-size: 12px; font-weight: 700; border: 2px solid var(--muted); color: transparent; }
  .box.on { background: var(--accent); border-color: var(--accent); color: #fff; }
  .lbody { flex: 1; min-width: 0; display: flex; align-items: center; gap: 10px; background: none; border: 0; padding: 0 10px 0 0; color: inherit; font: inherit; text-align: left; cursor: pointer; min-height: 56px; }
  .lbody:hover .lname { color: var(--accent); }
  .lthumb { flex: none; width: 84px; padding: 0; }
  .linfo { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; padding: 4px 0; }
  .lname { overflow-wrap: anywhere; }
  .lmeta { display: flex; flex-wrap: wrap; gap: 0 10px; align-items: center; }
  .rows.selecting .lbody { cursor: pointer; }
  .dests { display: grid; gap: 2px; max-height: 50dvh; overflow: auto; margin: 0 -4px; padding: 0 4px; }
  .dest { display: flex; gap: 10px; align-items: center; min-height: 44px; cursor: pointer; overflow-wrap: anywhere; }
  .newfolder { margin: 0 0 8px 28px; }
  @media (max-width: 760px) {
    .grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 4px; }
    .hidden-phone { display: none; }
  }
</style>
