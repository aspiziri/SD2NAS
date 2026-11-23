<script>
  let { health, error } = $props();
</script>

{#if error}
  <div class="note bad" role="alert">SD2NAS is not reachable ({error}). Checking again every minute.</div>
{:else if health && health.status !== 'ok'}
  <div class="note bad" role="alert">
    <b>SD2NAS needs attention.</b> Imports and staging may be unavailable until this is fixed:
    <ul>
      {#each health.problems ?? [] as p}<li>{p}</li>{/each}
    </ul>
  </div>
{/if}
{#if health?.warnings?.length}
  <div class="note warn">
    {#each health.warnings as w}<div>{w}</div>{/each}
  </div>
{/if}

<style>
  ul { margin: 6px 0 0; padding-left: 18px; }
</style>
