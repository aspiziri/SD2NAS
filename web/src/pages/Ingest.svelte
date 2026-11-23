<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatCount, formatWhen, formatBytes, statusLabel, statusClass, cardName, knownCount } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';

  const PAGE = 20;
  let ingests = $state([]);
  let loading = $state(true);
  let busy = $state(false); // a request is in flight
  let error = $state('');
  let unavailable = $state(false); // the server has no ingest routes (catalog can't open)
  let more = $state(false);
  let ctl = null; // the in-flight request; starting another aborts it

  // run fetches the history with limit n (after the id `before`, if given) and
  // hands the rows to apply. A newer run aborts this one, and its AbortError is
  // not an error. A failure keeps whatever is already on screen.
  async function run(n, before, apply) {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    busy = true;
    try {
      const data = await api(`/api/ingests?limit=${n}` + (before ? `&before=${before}` : ''), { signal: mine.signal });
      apply(data.ingests);
      more = data.ingests.length === n;
      error = '';
      unavailable = false;
    } catch (err) {
      if (err.name === 'AbortError') return;
      unavailable = !ingests.length && (err.status === 404 || err.status === 503);
      error = err.message;
    } finally {
      if (ctl === mine) {
        busy = false;
        loading = false;
      }
    }
  }

  // Refresh keeps as many rows as are shown, and replaces them at once.
  const refresh = () => {
    const n = Math.min(Math.max(PAGE, ingests.length), 1000);
    return run(n, 0, (rows) => (ingests = rows));
  };
  const loadMore = () =>
    run(PAGE, ingests[ingests.length - 1].id, (rows) => {
      const have = new Set(ingests.map((i) => i.id));
      ingests = [...ingests, ...rows.filter((i) => !have.has(i.id))];
    });

  // Load on mount, again when an import finishes, and again when the live
  // stream comes back after a drop: events missed while it was down are not
  // replayed. The first connection of a fresh page is not a reconnect.
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      if (!started || reconnected || finished) {
        started = true;
        refresh();
      }
    });
  });
  $effect(() => () => ctl?.abort());
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
    {#if error && ingests.length}
      <div class="note bad" role="alert">Couldn't refresh the import history: {error}</div>
    {/if}
    {#if error && !ingests.length}
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
                <td><span class="pill {statusClass(i)}" title={i.message}>{statusLabel(i.status)}</span>{#if i.forgotten_at}<span class="pill" title="Forgotten {formatWhen(i.forgotten_at)}">Forgotten</span>{/if}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <ul class="list only-phone">
        {#each ingests as i (i.id)}
          <li>
            <a href="/ingest/{i.id}">
              <span class="row"><b>{cardName(i)}</b><span class="spacer"></span><span class="pill {statusClass(i)}">{statusLabel(i.status)}</span>{#if i.forgotten_at}<span class="pill" title="Forgotten {formatWhen(i.forgotten_at)}">Forgotten</span>{/if}</span>
              <span class="muted small">{formatWhen(i.started_at)} · {formatCount(i.counts.new)} new · {formatCount(knownCount(i.counts))} known</span>
            </a>
          </li>
        {/each}
      </ul>
      {#if more}
        <p><button class="btn" disabled={busy} onclick={loadMore}>Load more</button></p>
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
