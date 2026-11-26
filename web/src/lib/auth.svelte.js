// Whether this SD2NAS asks for a password, and whether this browser has it.
import { api, setUnauthorizedHandler } from './api.js';

export const auth = $state({ checked: false, required: false, authorized: true });

// checkAuth asks GET /api/auth. A failure (network) leaves the app showing as
// it does without a password: the API's own errors surface on each page.
export async function checkAuth() {
  try {
    const a = await api('/api/auth');
    auth.required = !!a?.required;
    auth.authorized = !!a?.authorized;
  } catch {
    auth.required = false;
    auth.authorized = true;
  } finally {
    auth.checked = true;
  }
}

// A 401 token_required from any call: the cookie expired or the password changed.
setUnauthorizedHandler(() => {
  auth.required = true;
  auth.authorized = false;
});
