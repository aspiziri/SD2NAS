// Folder names follow the archive: "YYYY-MM-DD - Title" (PLAN §8). This
// mirrors internal/staging/names.go, so dialogs preview exactly what the
// server will do; the server still checks everything.
const NAME_RE = /^([0-9]{4}-[0-9]{2}-[0-9]{2})(?: - (.+))?$/;
const BAD_CHARS = /[/\\<>:"|?*\u0000-\u001f\u007f-\u009f]/;
const TOKEN_RE = /\{[^{}]*\}/g;
const encoder = new TextEncoder();

export const MAX_NAME_BYTES = 200;
export const DEFAULT_PATTERN = '{name}';

// Names are text: the date is never checked as a calendar date.
export function splitName(name) {
  const m = NAME_RE.exec(name);
  return m ? { date: m[1], title: m[2] ?? '' } : { date: '', title: name };
}

export function folderName(date, title) {
  if (!date) return title;
  if (!title) return date;
  return `${date} - ${title}`;
}

export function folderDate(folder) {
  return splitName(folder.name).date || (folder.dates?.[0] ?? '');
}

export function earliestDate(folders) {
  let best = '';
  for (const f of folders) {
    const d = folderDate(f);
    if (d && (!best || d < best)) best = d;
  }
  return best;
}

// The same rules and words as checkName in internal/staging/names.go.
export function nameProblem(s) {
  if (s === '') return 'is empty';
  if (s === '.' || s === '..') return 'is not a folder name';
  if (s.startsWith('.')) return 'starts with a dot (it would be hidden)';
  if (s.endsWith('.') || s.endsWith(' ')) return "ends with a dot or a space, which Windows can't use";
  if (encoder.encode(s).length > MAX_NAME_BYTES) return `is longer than ${MAX_NAME_BYTES} bytes`;
  if (!s.isWellFormed()) return 'is not valid UTF-8'; // a lone surrogate
  const m = BAD_CHARS.exec(s);
  if (m) return `contains ${JSON.stringify(m[0])}, which can't be used in a folder name`;
  return '';
}

// date is optional: when given, the whole "date - title" name must fit the
// byte limit too, as the server checks it.
export function checkTitle(raw, date = '') {
  const title = raw.trim();
  if (title === '') return { title, error: '' };
  const p = nameProblem(title);
  if (p) return { title, error: `The title ${p}.` };
  const whole = nameProblem(folderName(date, title));
  if (whole) return { title, error: `The folder name ${whole}.` };
  return { title, error: '' };
}

export function promoteTarget(folder, pattern) {
  const p = pattern.trim() || DEFAULT_PATTERN;
  const date = folderDate(folder);
  let error = '';
  const path = p.replace(TOKEN_RE, (tok) => {
    if (tok === '{name}') return folder.name;
    if (tok === '{year}' || tok === '{month}') {
      if (!date) {
        error ||= `${tok} needs a date, and “${folder.name}” has none`;
        return '';
      }
      if (date.length < 10 || date[4] !== '-' || date[7] !== '-') {
        error ||= `${tok} needs a YYYY-MM-DD date, and “${folder.name}” has “${date}”`;
        return '';
      }
      return tok === '{year}' ? date.slice(0, 4) : date.slice(5, 7);
    }
    error ||= `Unknown token ${tok} (use {name}, {year} or {month})`;
    return '';
  });
  if (error) return { path: '', error };
  if (/[{}]/.test(path)) return { path: '', error: 'The pattern has unbalanced braces' };
  for (const el of path.split('/')) {
    const prob = nameProblem(el);
    if (prob) return { path: '', error: `In “${path}” a folder name ${prob}` };
  }
  return { path, error: '' };
}
