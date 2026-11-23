<script>
  import { live } from '../lib/live.svelte.js';
  import { phaseSteps, progressFraction, formatCount, formatBytes, cardName, knownCount, levelClass, addSample, copyEstimate, formatTimeLeft } from '../lib/format.js';

  const running = $derived(live.current.running);
  const queued = $derived(live.current.queued);
  const p = $derived(running?.progress ?? null);
  const steps = $derived(phaseSteps(p?.phase ?? 'queued'));
  const pct = $derived(Math.round(progressFraction(p) * 100));
  const last = $derived(live.finished);

  // Copy speed and time left, from the progress events of the current job.
  // The samples are plain variables: only the estimate is shown.
  let samples = [];
  let sampledJob = null;
  let estimate = $state(null);
  $effect(() => {
    if (!running || p?.phase !== 'copying') {
      samples = [];
      sampledJob = null;
      estimate = null;
      return;
    }
    if (running.id !== sampledJob) {
      samples = [];
      sampledJob = running.id;
    }
    samples = addSample(samples, Date.now(), p.bytes_done);
    estimate = copyEstimate(samples, p.bytes_total - p.bytes_done);
  });
</script>

<div class="card">
  {#if running}
    <div class="row">
      <h2 style="margin:0">{cardName(running)}</h2>
      <span class="muted small mono">{running.card_path}</span>
    </div>
    <div class="phases">
      {#each steps as s (s.key)}
        <span class="phase {s.state}">{s.state === 'done' ? '✓' : s.state === 'current' ? '●' : '○'} {s.label}</span>
      {/each}
    </div>
    <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={pct}><i style:width="{pct}%"></i></div>
    <div class="small muted">
      {#if p?.phase === 'classifying'}
        {formatCount(p.files_done)} / {formatCount(p.files_total)} files checked
      {:else if p?.phase === 'copying'}
        {formatCount(p.files_done)} / {formatCount(p.files_total)} new files copied · {formatBytes(p.bytes_done)} of {formatBytes(p.bytes_total)}
        {#if estimate}<br>{formatBytes(estimate.rate)}/s · {formatTimeLeft(estimate.seconds)}{/if}
      {:else if p?.phase === 'scanning'}
        Reading the card's file list…
      {:else}
        Finishing…
      {/if}
    </div>
    {#if p}
      <div class="stats" style="margin-top:12px">
        <div class="stat acc"><b>{formatCount(p.counts.new)}</b>new</div>
        <div class="stat"><b>{formatCount(knownCount(p.counts))}</b>already known</div>
        {#if p.counts.failed}<div class="stat bad"><b>{formatCount(p.counts.failed)}</b>failed</div>{/if}
        {#if p.counts.unsupported}<div class="stat warn"><b>{formatCount(p.counts.unsupported)}</b>not imported</div>{/if}
      </div>
    {/if}
  {:else}
    <div class="waiting">
      <h2 style="margin:0 0 4px">No card importing</h2>
      <p class="muted" style="margin:0">Insert an SD card into the NAS. Unassigned Devices mounts it and the import starts on its own.</p>
    </div>
  {/if}
  {#if queued.length}
    <p class="small muted">Waiting: {queued.map(cardName).join(', ')}</p>
  {/if}
  {#if last && !running}
    <div class="note {levelClass(last.level)}">
      <b>Last import:</b> {last.message} <a href="/ingest/{last.id}">Details</a>
    </div>
  {/if}
  {#if !live.connected}<p class="small muted">Reconnecting to live updates…</p>{/if}
</div>
