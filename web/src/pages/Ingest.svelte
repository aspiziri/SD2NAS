<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatCount, formatWhen, formatBytes, statusLabel, statusClass, cardName, knownCount } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';

  const PAGE = 20;
  let ingests = $state([]);
  let loading = $state(true);
  let error = $state('');
  let unavailable = $state(false); // the server has no ingest routes (catalog can't open)
  let more = $state(false);

  async function load(before = 0) {
    try {
      const data = await api(`/api/ingests?limit=${PAGE}` + (before ? `&before=${before}` : ''));
      ingests = before ? [...ingests, ...data.ingests] : data.ingests;
      more = data.ingests.length === PAGE;
      error = '';
      unavailable = false;
    } catch (err) {
      unavailable = err.status === 404 || err.status === 503;
      error = err.message;
    } finally {
      loading = false;
    }
  }

  // Load on mount, again when an import finishes, and again whenever the live
  // stream reconnects: events missed while it was down are not replayed.
  let started = false;
  $effect(() => {
    live.finishedCount;
    const connected = live.connected;
    untrack(() => {
      if (connected || !started) {
        started = true;
        load();
      }
    });
  });
</script>

<h1>Card ingest</h1>
<p class="sub">Imports start on their own when a card is mounted. Nothing is ever deleted from the card.</p>

{#if unavailable}
  <div class="note bad" role="alert">
    <b>Ingest is unavailable.</b> The server can't open its catalog, so imports can't run and no history can be shown. The notice at the top of the page says what to fix.
  </div>
{:else}
  <LiveIngest />

  <div class="card">
    <h2>Recent imports</h2>
    {#if error}
      <div class="note bad">Couldn't load the import history: {error}</div>
    {:else if loading}
      <p class="muted">Loading…</p>
    {:else if ingests.length === 0}
      <p class="muted">No cards have been imported yet.</p>
    {:else}
      <div class="only-desktop" style="overflow-x:auto">
        <table>
          <thead>
            <tr><th>When</th><th>Card</th><th class="num">On card</th><th class="num">New</th><th class="num">Known</th><th class="num">Seen before</th><th>Status</th></tr>
          </thead>
          <tbody>
            {#each ingests as i (i.id)}
              <tr>
                <td><a href="/ingest/{i.id}">{formatWhen(i.started_at)}</a></td>
                <td>{cardName(i)}{#if i.card_size}<span class="muted small"> {formatBytes(i.card_size)}</span>{/if}</td>
                <td class="num">{formatCount(i.counts.seen)}</td>
                <td class="num" style="color:var(--ok)">{formatCount(i.counts.new)}</td>
                <td class="num">{formatCount(i.counts.in_archive + i.counts.in_staging)}</td>
                <td class="num">{formatCount(i.counts.seen_before)}</td>
                <td><span class="pill {statusClass(i)}" title={i.message}>{statusLabel(i.status)}</span></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <ul class="list only-phone">
        {#each ingests as i (i.id)}
          <li>
            <a href="/ingest/{i.id}">
              <span class="row"><b>{cardName(i)}</b><span class="spacer"></span><span class="pill {statusClass(i)}">{statusLabel(i.status)}</span></span>
              <span class="muted small">{formatWhen(i.started_at)} · {formatCount(i.counts.new)} new · {formatCount(knownCount(i.counts))} known</span>
            </a>
          </li>
        {/each}
      </ul>
      {#if more}
        <p><button class="btn" onclick={() => load(ingests[ingests.length - 1].id)}>Load more</button></p>
      {/if}
    {/if}
  </div>
{/if}

<style>
  .list { list-style: none; margin: 0; padding: 0; }
  .list li + li { border-top: 1px solid var(--line); }
  .list a { display: flex; flex-direction: column; gap: 2px; padding: 10px 2px; color: var(--text); text-decoration: none; min-height: 44px; }
  tbody tr:hover { background: var(--panel2); }
</style>
