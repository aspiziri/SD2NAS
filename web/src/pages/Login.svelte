<script>
  import { api } from '../lib/api.js';
  import { describeError } from '../lib/messages.js';

  let password = $state('');
  let busy = $state(false);
  let error = $state('');

  async function submit(e) {
    e.preventDefault();
    if (busy) return;
    busy = true;
    error = '';
    try {
      await api('/api/login', { method: 'POST', body: { token: password } });
      location.reload();
    } catch (err) {
      error = err.status === 401 ? 'That password is not right.' : describeError(err);
      busy = false;
    }
  }
</script>

<div class="wrap">
  <form class="card login" onsubmit={submit}>
    <h1>SD2NAS</h1>
    <p class="sub">This SD2NAS is protected by a password. Enter it once; this browser remembers it for a year.</p>
    <label class="f" for="pw">Password</label>
    <input id="pw" type="password" autocomplete="current-password" bind:value={password} aria-describedby="pw-hint" aria-invalid={error ? 'true' : undefined} />
    <div class="hint" id="pw-hint">It's the SD2NAS_TOKEN value in the container's settings.</div>
    {#if error}<div class="field-error" role="alert">{error}</div>{/if}
    <button class="btn primary" type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
  </form>
</div>

<style>
  .wrap { min-height: 100vh; min-height: 100dvh; display: flex; align-items: center; justify-content: center; padding: 16px; box-sizing: border-box; }
  .login { width: 100%; max-width: 400px; margin: 0; display: flex; flex-direction: column; }
  h1 { margin: 0 0 8px; }
  input { background: var(--bg); color: var(--text); border: 1px solid var(--line); border-radius: 7px; padding: 9px 10px; font: inherit; width: 100%; min-height: 44px; box-sizing: border-box; }
  .hint { color: var(--muted); font-size: 12px; margin: 6px 0 16px; }
  .field-error { margin: -8px 0 12px; }
  .btn { justify-content: center; }
</style>
