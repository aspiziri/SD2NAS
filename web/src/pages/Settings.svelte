<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { describeError } from '../lib/messages.js';
  import { toast } from '../lib/toast.svelte.js';
  import { FIELDS, changedKeys, fieldOfError, checkField } from '../lib/settings.js';
  import { loadPref, savePref, PATTERN_KEY } from '../lib/prefs.js';
  import { promoteTarget, DEFAULT_PATTERN } from '../lib/naming.js';

  let loaded = $state(null); // settings as the server has them
  let defaults = $state({});
  let form = $state({});
  let loadError = $state('');
  let saving = $state(false);
  let saveError = $state(''); // a 400 sentence that names no field
  let serverErrors = $state({}); // key -> the server's sentence
  let authRequired = $state(false);
  let signingOut = $state(false);

  let pattern = $state(loadPref(PATTERN_KEY, null, DEFAULT_PATTERN));
  let patternSaved = $state(pattern);

  const changed = $derived(loaded ? changedKeys(loaded, form) : {});
  const dirty = $derived(Object.keys(changed).length > 0);
  const sample = { name: '2024-05-01 - Yosemite' };
  const patternTarget = $derived(promoteTarget(sample, pattern));
  const backupIsDefault = $derived(loaded !== null && loaded.backup_dir === defaults.backup_dir);

  function adopt(res) {
    loaded = { ...res.settings };
    defaults = { ...res.defaults };
    form = { ...res.settings };
    serverErrors = {};
    saveError = '';
  }

  let ctl = null; // the load in flight; a newer one aborts it
  async function load() {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api('/api/settings', { signal: mine.signal });
      if (mine.signal.aborted) return;
      adopt(res);
      loadError = '';
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      loadError = describeError(err); // the last good data stays on screen
    }
  }

  onMount(() => {
    load();
    return () => ctl?.abort();
  });
  onMount(() => {
    api('/api/auth').then((a) => { authRequired = !!a?.required; }).catch(() => { authRequired = false; });
  });

  // The message under a field: the server's sentence if it refused this
  // value, else the advisory check (which never blocks Save).
  function problem(key) {
    if (serverErrors[key]) return serverErrors[key];
    return changed[key] !== undefined ? checkField(key, form[key]) : '';
  }

  function edit(key) {
    if (serverErrors[key]) {
      const rest = { ...serverErrors };
      delete rest[key];
      serverErrors = rest;
    }
  }

  async function save() {
    saving = true;
    saveError = '';
    serverErrors = {};
    try {
      await api('/api/settings', { method: 'PUT', body: changed });
      toast('Settings saved');
      await load();
    } catch (err) {
      const key = err.status === 400 ? fieldOfError(err.message) : '';
      if (key && key in form) serverErrors = { [key]: err.message };
      else saveError = describeError(err);
    } finally {
      saving = false;
    }
  }

  function discard() {
    form = { ...loaded };
    serverErrors = {};
    saveError = '';
  }

  function savePattern() {
    const p = pattern.trim() || DEFAULT_PATTERN;
    if (savePref(PATTERN_KEY, p)) {
      pattern = patternSaved = p;
      toast('Default promote pattern saved on this device');
    } else {
      toast('This browser would not store the pattern. It lasts until the page closes.', 'warn');
    }
  }

  async function signOut() {
    signingOut = true;
    try {
      await api('/api/logout', { method: 'POST' });
      location.reload();
    } catch (err) {
      signingOut = false;
      toast(describeError(err), 'bad');
    }
  }
</script>

<h1>Settings</h1>
<p class="sub">Changes apply at once. Save sends only what you changed.</p>

{#snippet hint(key)}
  {@const msg = problem(key)}
  <div class="muted small">{FIELDS[key].help} Default {defaults[key]}.</div>
  {#if msg}<div class="field-error" role="alert">{msg}</div>{/if}
{/snippet}

{#if loadError}
  <div class="note bad" role="alert">{loaded ? "Could not refresh the settings: " : ""}{loadError} <button class="btn" onclick={load}>Retry</button></div>
{/if}
{#if !loaded}
  {#if !loadError}<p class="muted">Loading…</p>{/if}
{:else}
  <fieldset class="plain" disabled={saving}>
  <div class="card">
    <h2>Imports</h2>
    <div class="form2">
      <div>
        <label class="f" for="s-day_starts_at">{FIELDS.day_starts_at.label}</label>
        <input id="s-day_starts_at" type="time" bind:value={form.day_starts_at} oninput={() => edit('day_starts_at')} />
        {@render hint('day_starts_at')}
      </div>
      <div>
        <label class="f" for="s-verify_known">{FIELDS.verify_known.label}</label>
        <select id="s-verify_known" bind:value={form.verify_known} onchange={() => edit('verify_known')}>
          <option value="quick">Quick</option>
          <option value="full">Full</option>
        </select>
        {@render hint('verify_known')}
      </div>
      <div>
        <label class="f" for="s-free_space_reserve_gb">{FIELDS.free_space_reserve_gb.label}</label>
        <input id="s-free_space_reserve_gb" type="text" inputmode="numeric" bind:value={form.free_space_reserve_gb} oninput={() => edit('free_space_reserve_gb')} />
        {@render hint('free_space_reserve_gb')}
      </div>
    </div>
  </div>

  <div class="card">
    <h2>Staging</h2>
    <div class="form2">
      <div>
        <label class="f" for="s-staging_warn_days">{FIELDS.staging_warn_days.label}</label>
        <input id="s-staging_warn_days" type="text" inputmode="numeric" bind:value={form.staging_warn_days} oninput={() => edit('staging_warn_days')} />
        {@render hint('staging_warn_days')}
      </div>
      <div>
        <label class="f" for="s-staging_notice_at">{FIELDS.staging_notice_at.label}</label>
        <input id="s-staging_notice_at" type="time" bind:value={form.staging_notice_at} oninput={() => edit('staging_notice_at')} />
        {@render hint('staging_notice_at')}
      </div>
    </div>
  </div>

  <div class="card">
    <h2>Archive index</h2>
    <div class="form2">
      <div>
        <label class="f" for="s-archive_rescan_at">{FIELDS.archive_rescan_at.label}</label>
        <input id="s-archive_rescan_at" type="time" bind:value={form.archive_rescan_at} oninput={() => edit('archive_rescan_at')} />
        {@render hint('archive_rescan_at')}
      </div>
      <div>
        <label class="f" for="s-backup_dir">{FIELDS.backup_dir.label}{#if backupIsDefault && form.backup_dir === loaded.backup_dir} <span class="pill">(default)</span>{/if}</label>
        <div class="row nowrap">
          <input id="s-backup_dir" type="text" bind:value={form.backup_dir} oninput={() => edit('backup_dir')} spellcheck="false" autocapitalize="off" />
          <button class="btn" type="button" onclick={() => { form.backup_dir = ''; edit('backup_dir'); }} disabled={form.backup_dir === ''}>Use default</button>
        </div>
        {#if form.backup_dir === ''}<div class="muted small">Saving an empty folder puts the backups in the default place.</div>{/if}
        {@render hint('backup_dir')}
      </div>
      <div class="wide">
        <label class="f" for="s-archive_skip">{FIELDS.archive_skip.label}</label>
        <textarea id="s-archive_skip" rows="3" bind:value={form.archive_skip} oninput={() => edit('archive_skip')} spellcheck="false" autocapitalize="off"></textarea>
        {@render hint('archive_skip')}
      </div>
    </div>
  </div>

  {#if saveError}<div class="note bad" role="alert">{saveError}</div>{/if}
  </fieldset>
  <div class="row savebar">
    <button class="btn primary" onclick={save} disabled={!dirty || saving}>{saving ? 'Saving…' : 'Save changes'}</button>
    <button class="btn" onclick={discard} disabled={!dirty || saving}>Discard</button>
  </div>
{/if}

<div class="card">
  <h2>This device</h2>
  <p class="muted small" style="margin:0 0 4px">Kept in this browser only.</p>
  <label class="f" for="s-pattern">Default promote pattern</label>
  <div class="row nowrap">
    <input id="s-pattern" type="text" bind:value={pattern} spellcheck="false" autocapitalize="off" />
    <button class="btn" type="button" onclick={savePattern} disabled={!!patternTarget.error || pattern.trim() === patternSaved}>Save pattern</button>
  </div>
  <div class="muted small">Tokens: <span class="mono">{'{year}'}</span> <span class="mono">{'{month}'}</span> <span class="mono">{'{name}'}</span>. The Staging promote dialog starts from it.</div>
  {#if patternTarget.error}
    <div class="field-error" role="alert">{patternTarget.error}</div>
  {:else}
    <div class="muted small">Example: <span class="mono">Archive/{patternTarget.path}/</span></div>
  {/if}
  {#if authRequired}
    <div class="row" style="margin-top:16px"><button class="btn" onclick={signOut} disabled={signingOut}>{signingOut ? 'Signing out…' : 'Sign out on this device'}</button></div>
  {/if}
</div>

<style>
  .form2 { display: grid; grid-template-columns: 1fr 1fr; gap: 0 18px; }
  .plain { border: 0; padding: 0; margin: 0; min-width: 0; max-width: 100%; inline-size: auto; }
  .form2 > div { min-width: 0; }
  .form2 .small, .field-error, label.f { overflow-wrap: anywhere; }
  input, textarea, select { width: 100%; min-width: 0; box-sizing: border-box; }
  .wide { grid-column: 1 / -1; }
  .nowrap { flex-wrap: nowrap; }
  .nowrap input { flex: 1; min-width: 0; }
  .savebar { margin-bottom: 16px; }
  @media (max-width: 760px) {
    .form2 { grid-template-columns: minmax(0, 1fr); }
  }
</style>
