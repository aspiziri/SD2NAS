<script>
  import { formatBytes, formatCount } from '../lib/format.js';
  import { promoteSummary, plural } from '../lib/messages.js';
  // queue: the promote.progress data { running, queued, last }.
  // showLast: whether the last finished job is still worth showing.
  let { queue, showLast, oncancel, ondismiss, cancelling = 0 } = $props();
  const icon = { waiting: '○', moving: '●', done: '✓', failed: '✗' };
  const pct = (j) => (j.files_total ? Math.round((j.files_done / j.files_total) * 100) : 0);
</script>

{#snippet folderList(job)}
  <ul>
    {#each job.folders as f (f.id)}
      <li class={f.state}>
        <span class="icon" aria-hidden="true">{icon[f.state]}</span>
        <span class="what"><b>{f.name}</b> → <span class="mono">{f.target}/</span>
          {#if f.merged}<span class="tag">merged into an existing folder</span>{/if}
          {#if f.copied}<span class="tag">copied across disks</span>{/if}
          {#if f.error}<span class="err">{f.error}</span>{/if}
        </span>
      </li>
    {/each}
  </ul>
{/snippet}

<div class="card promote" role="status">
  {#if queue.running}
    {@const j = queue.running}
    <h2>Promoting to the archive</h2>
    <p class="muted small">{promoteSummary(j)}{#if j.bytes_total}{' '}· {formatBytes(j.bytes_done)} of {formatBytes(j.bytes_total)}{/if}</p>
    <div class="bar" aria-hidden="true"><i style="width:{pct(j)}%"></i></div>
    {@render folderList(j)}
  {/if}
  {#if queue.queued.length}
    <h3>Waiting ({plural(queue.queued.length, 'promote', 'promotes')})</h3>
    {#each queue.queued as j (j.id)}
      <div class="queued">
        <div class="row">
          <span class="muted small">{plural(j.folders.length, 'folder', 'folders')} · {formatCount(j.files_total)} files</span>
          <span class="spacer"></span>
          <button class="btn" disabled={cancelling === j.id} onclick={() => oncancel(j)}>{cancelling === j.id ? 'Cancelling…' : 'Cancel'}</button>
        </div>
        {@render folderList(j)}
      </div>
    {/each}
  {/if}
  {#if showLast && queue.last}
    {@const j = queue.last}
    <div class="last" class:failed={j.state === 'failed'}>
      <div class="row">
        <h3>{j.state === 'failed' ? 'Last promote finished with problems' : 'Last promote'}</h3>
        <span class="spacer"></span>
        <button class="btn link" onclick={() => ondismiss(j)}>Dismiss</button>
      </div>
      <p class="muted small">{promoteSummary(j)}</p>
      {#if j.state === 'failed'}{@render folderList(j)}{/if}
    </div>
  {/if}
</div>

<style>
  h2 { margin: 0 0 2px; }
  h3 { font-size: 13px; margin: 12px 0 4px; }
  .muted.small { margin: 0; }
  .queued, .last { border-top: 1px solid var(--line); margin-top: 10px; padding-top: 6px; }
  .last.failed h3 { color: var(--bad); }
  ul { list-style: none; margin: 8px 0 0; padding: 0; display: grid; gap: 6px; }
  li { display: flex; gap: 8px; align-items: baseline; }
  .icon { width: 1.2em; flex: none; color: var(--muted); }
  li.done .icon { color: var(--ok); } li.failed .icon { color: var(--bad); } li.moving .icon { color: var(--accent); }
  .what { min-width: 0; overflow-wrap: anywhere; }
  .err { display: block; color: var(--bad); font-size: 12px; }
</style>
