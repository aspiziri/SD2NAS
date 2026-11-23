<script>
  import { untrack } from 'svelte';
  import { formatBytes, formatCapture, extLabel } from '../lib/format.js';

  let { files, index = $bindable(), onclose } = $props();

  const file = $derived(files[index]);
  let loaded = $state(false);
  let failed = $state(false);
  let chrome = $state(true); // top and bottom bars

  const sha = $derived(file.sha256);

  // A new file (not merely a refetched list): reset the load state and warm
  // the neighbours' previews.
  $effect(() => {
    sha;
    loaded = false;
    failed = false;
    untrack(() => {
      for (const j of [index - 1, index + 1]) {
        if (j >= 0 && j < files.length) new Image().src = `/api/preview/${files[j].sha256}`;
      }
    });
  });

  let dialog;

  // Modal: lock scroll, make the page behind inert, focus the dialog, and
  // give focus back to whatever opened it on close.
  $effect(() => {
    const opener = document.activeElement;
    const app = document.querySelector('.app');
    document.body.classList.add('noscroll');
    if (app) app.inert = true;
    dialog.focus();
    return () => {
      document.body.classList.remove('noscroll');
      if (app) app.inert = false;
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  // Render the dialog directly under <body> so the inert page doesn't contain it.
  function portal(node) {
    document.body.appendChild(node);
  }

  function trapTab(e) {
    const items = [...dialog.querySelectorAll('button:not(:disabled)')];
    if (items.length === 0) {
      e.preventDefault();
      dialog.focus();
      return;
    }
    const at = items.indexOf(document.activeElement);
    const to = e.shiftKey ? (at <= 0 ? items.length - 1 : at - 1) : at === items.length - 1 ? 0 : at + 1;
    e.preventDefault();
    items[to].focus();
  }

  function prev() {
    if (index > 0) index -= 1;
  }
  function next() {
    if (index < files.length - 1) index += 1;
  }

  function onkeydown(e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === 'Tab') {
      trapTab(e);
      return;
    }
    if (e.key === 'ArrowLeft') prev();
    else if (e.key === 'ArrowRight') next();
    else if (e.key === 'Escape') onclose();
    else return;
    e.preventDefault();
  }

  // Swipe left or right for the next or previous file, down to close. A tap
  // on the left or right third steps; in the middle it shows or hides the bars.
  let start = null;
  function onpointerdown(e) {
    if (e.isPrimary) start = { x: e.clientX, y: e.clientY };
  }
  function onpointerup(e) {
    if (!start || !e.isPrimary) return;
    const dx = e.clientX - start.x;
    const dy = e.clientY - start.y;
    start = null;
    if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy)) {
      if (dx < 0) next();
      else prev();
    } else if (dy > 80 && dy > Math.abs(dx)) {
      onclose();
    } else if (Math.abs(dx) < 10 && Math.abs(dy) < 10) {
      const third = e.currentTarget.clientWidth / 3;
      if (e.clientX < third) prev();
      else if (e.clientX > 2 * third) next();
      else chrome = !chrome;
    }
  }
</script>

<svelte:window {onkeydown} />

<div class="lightbox" role="dialog" aria-modal="true" aria-label={file.name} tabindex="-1" bind:this={dialog} use:portal>
  <div class="stage" role="presentation" {onpointerdown} {onpointerup} onpointercancel={() => (start = null)}>
    {#key sha}
      <img src="/api/preview/{file.sha256}" alt={file.name} draggable="false" class:hidden={!loaded}
        onload={() => (loaded = true)} onerror={() => (failed = true)} />
    {/key}
    {#if failed}
      <div class="msg">No preview for this file.</div>
    {:else if !loaded}
      <div class="msg">Loading…</div>
    {/if}
  </div>
  {#if chrome}
    <div class="top">
      <button class="icon" onclick={onclose} aria-label="Close">✕</button>
      <span>{index + 1} / {files.length}</span>
      {#if file.kind === 'video'}<span class="pill">Video · frame at 1 s</span>{/if}
    </div>
    <div class="bottom">
      <div class="fname">{file.name}</div>
      <div class="meta">
        {file.capture_time ? formatCapture(file.capture_time) : 'No capture time'}{file.tz_offset ? ` (UTC${file.tz_offset})` : ''} · {formatBytes(file.size)} · {extLabel(file)}
      </div>
      {#each file.clock_warnings ?? [] as w}<div class="warn">⚠ {w}</div>{/each}
    </div>
    <button class="step prev" onclick={prev} disabled={index === 0} aria-label="Previous">‹</button>
    <button class="step next" onclick={next} disabled={index === files.length - 1} aria-label="Next">›</button>
  {/if}
</div>

<style>
  .lightbox:focus { outline: none; }
  .lightbox { position: fixed; inset: 0; z-index: 50; background: #000; color: var(--text); }
  .stage { position: absolute; inset: 0; display: grid; place-items: center; touch-action: none; user-select: none; }
  .stage img { max-width: 100%; max-height: 100%; object-fit: contain; -webkit-user-drag: none; }
  .hidden { visibility: hidden; }
  .msg { position: absolute; color: var(--muted); }
  .top { position: absolute; top: 0; left: 0; right: 0; display: flex; align-items: center; gap: 12px; padding: calc(8px + env(safe-area-inset-top)) calc(12px + env(safe-area-inset-right)) 8px calc(12px + env(safe-area-inset-left)); background: linear-gradient(rgba(0,0,0,.7), transparent); }
  .bottom { position: absolute; left: 0; right: 0; bottom: 0; padding: 16px calc(16px + env(safe-area-inset-right)) calc(14px + env(safe-area-inset-bottom)) calc(16px + env(safe-area-inset-left)); background: linear-gradient(transparent, rgba(0,0,0,.75)); font-size: 13px; }
  .fname { font-weight: 600; overflow-wrap: anywhere; }
  .meta { color: var(--muted); }
  .warn { color: var(--warn); }
  .icon { background: rgba(255,255,255,.12); border: 0; color: #fff; width: 44px; height: 44px; border-radius: 50%; font-size: 18px; cursor: pointer; }
  .step { position: absolute; top: 50%; transform: translateY(-50%); background: rgba(255,255,255,.12); border: 0; color: #fff; width: 44px; height: 64px; font-size: 32px; border-radius: 8px; cursor: pointer; }
  .step:hover:not(:disabled) { background: rgba(255,255,255,.25); }
  .step:disabled { opacity: .25; cursor: default; }
  .prev { left: calc(8px + env(safe-area-inset-left)); }
  .next { right: calc(8px + env(safe-area-inset-right)); }
  @media (hover: none) { .step { display: none; } }
</style>
