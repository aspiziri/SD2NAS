// The Settings page's field table and its advisory checks. The checks mirror
// the Go rules in internal/catalog/settings.go so a typo shows at once; they
// never block Save, because the server's answer is the one that counts.

const CLOCK = /^([01]\d|2[0-3]):[0-5]\d$/;
const BACKSLASH = String.fromCharCode(92);
const clock =(v) => (CLOCK.test(v) ? '' : 'Use HH:MM, 00:00 to 23:59.');

// key -> label, help text. Order is the page's order within each card.
export const FIELDS = {
  day_starts_at: { label: 'Day starts at', help: 'Shots taken before this time count as the previous day.' },
  verify_known: { label: 'Check known files', help: 'Quick compares size and date; Full reads every byte.' },
  free_space_reserve_gb: { label: 'Free-space reserve (GB)', help: 'An import stops before it would leave less than this free.' },
  staging_warn_days: { label: 'Flag folders older than (days)', help: '0 turns the flag off.' },
  staging_notice_at: { label: 'Daily notice at', help: 'When the stale-folder notice is checked each day.' },
  archive_rescan_at: { label: 'Nightly scan and backup at', help: 'The archive is scanned and the index backed up then.' },
  backup_dir: { label: 'Backup folder', help: 'Must be outside Archive/ and Staging/.' },
  archive_skip: { label: 'Skip in the archive', help: 'Comma-separated names or patterns, such as Output/, *.xmp.' },
};

// checkField returns '' when value looks fine, else a short message.
export function checkField(key, value) {
  switch (key) {
    case 'day_starts_at':
    case 'archive_rescan_at':
    case 'staging_notice_at':
      return clock(value);
    case 'verify_known':
      return value === 'quick' || value === 'full' ? '' : 'Choose Quick or Full.';
    case 'free_space_reserve_gb':
      return /^\d+$/.test(value) ? '' : 'Use a whole number of GB.';
    case 'staging_warn_days':
      return /^\d+$/.test(value) && Number(value) <= 3650 ? '' : 'Use 0 (off) to 3650 days.';
    case 'backup_dir':
      return value === '' || value.startsWith('/') || /^[A-Za-z]:/.test(value) && value[2] === BACKSLASH ? '' : 'Use an absolute path, or Use default.';
    case 'archive_skip': {
      const entries = value.split(',').map((e) => e.trim()).filter(Boolean);
      const bad = value.length > 2048 || [...value].some((c) => c.charCodeAt(0) < 32 || c.charCodeAt(0) === 127) || entries.length > 50
        || entries.some((e) => { const p = e.replace(/\/$/, ''); return p === '' || p.includes('/') || p.includes(BACKSLASH); });
      return bad ? 'Use names or patterns separated by commas, with no slashes inside a name.' : '';
    }
    default:
      return '';
  }
}

// changedKeys: the entries of form that differ from loaded.
export function changedKeys(loaded, form) {
  const out = {};
  for (const [k, v] of Object.entries(form)) if (loaded[k] !== v) out[k] = v;
  return out;
}

// fieldOfError names the setting a 400 sentence is about, or ''.
export function fieldOfError(message) {
  const m = /^Setting ([a-z_0-9]+):/.exec(message) ?? /^Unknown setting (\S+?)\.$/.exec(message);
  return m ? m[1] : '';
}
