// Per-device preferences in localStorage. Storage can be missing or throw
// (private mode, blocked site data): reads fall back, writes report false.

export const PATTERN_KEY = 'sd2nas.promotePattern';
export const STAGING_VIEW_KEY = 'sd2nas.stagingView';
export const FOLDER_VIEW_KEY = 'sd2nas.folderView';

// loadPref returns the stored value if it is one of `allowed` (any non-empty
// value when allowed is null), else the fallback.
export function loadPref(key, allowed, fallback) {
  try {
    const v = localStorage.getItem(key);
    if (v === null || v === undefined || v === '') return fallback;
    return allowed === null || allowed.includes(v) ? v : fallback;
  } catch {
    return fallback; // storage unavailable: use the default
  }
}

export function savePref(key, value) {
  try {
    localStorage.setItem(key, value);
    return true;
  } catch {
    return false; // storage unavailable: the choice lasts until the page closes
  }
}
