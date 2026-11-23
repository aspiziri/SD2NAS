<script>
  let { sha, kind, label = '' } = $props();
  let failed = $state(false); // /api/thumbs answered an error: show the placeholder, never retry
  const badge = $derived(label || (kind === 'raw' ? 'RAW' : kind === 'video' ? 'VIDEO' : 'JPG'));
  const icon = $derived(kind === 'raw' ? 'RAW' : kind === 'video' ? '🎞' : '🖼');
</script>

<div class="thumb">
  {#if failed}
    <span class="ph" aria-hidden="true">{icon}</span>
  {:else}
    <img src="/api/thumbs/{sha}" alt="" loading="lazy" decoding="async" onerror={() => (failed = true)} />
  {/if}
  {#if kind === 'video'}<span class="play" aria-hidden="true">▶</span>{/if}
  <span class="b">{badge}</span>
</div>

<style>
  .thumb { aspect-ratio: 3 / 2; border-radius: 5px; position: relative; overflow: hidden; background: var(--panel2); }
  img { width: 100%; height: 100%; object-fit: cover; display: block; }
  .ph { position: absolute; inset: 0; display: grid; place-items: center; font-size: 15px; font-weight: 700; color: var(--muted); }
  .b { position: absolute; left: 4px; bottom: 3px; font-size: 9px; font-weight: 700; background: rgba(0,0,0,.55); color: #fff; padding: 0 4px; border-radius: 3px; }
  .play { position: absolute; inset: 0; display: grid; place-items: center; font-size: 16px; color: #fff; opacity: .85; }
</style>
