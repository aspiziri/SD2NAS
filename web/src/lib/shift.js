// The Shift dialog's preview. A preview only (Decision 27): the server decides.
// Capture times are naive wall clocks ("2025-05-31T01:30:12"), so all the
// arithmetic is on UTC fields, never through the browser's zone or its DST
// (Go's shiftWall does the same). Rules mirror internal/staging/shift.go.
import { splitName, folderName } from './naming.js';

export const MAX_SHIFT = 3155760000; // seconds: 100 years of 365.25 days
export const MAX_SHIFT_FILES = 20000; // staging.MaxFiles

const WALL = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?/;
const pad = (n, w = 2) => String(n).padStart(w, '0');

// A capture wall clock (seconds optional) as milliseconds on the UTC clock; NaN if it isn't one.
function wallMs(s) {
  const m = WALL.exec(s ?? '');
  if (!m) return NaN;
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], m[6] ? +m[6] : 0);
}

function wallText(ms) {
  const d = new Date(ms);
  return `${pad(d.getUTCFullYear(), 4)}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`;
}

// parseAmount turns the dialog's four fields into signed seconds, or an error sentence.
export function parseAmount({ sign, days, hours, minutes, seconds }) {
  const parts = [days, hours, minutes, seconds].map((v) => (v ?? '').trim());
  if (parts.some((v) => !/^\d*$/.test(v))) return { error: 'Use whole numbers.' };
  const [d, h, m, s] = parts.map((v) => (v === '' ? 0 : Number(v)));
  const total = d * 86400 + h * 3600 + m * 60 + s;
  if (total === 0) return { error: 'Shift by at least one second.' };
  if (total > MAX_SHIFT) return { error: 'Shift by at most 100 years.' };
  return { seconds: sign === '-' ? -total : total };
}

// secondsBetween is how far to shift a file so that its time `from` reads `to`.
export function secondsBetween(from, to) {
  return (wallMs(to) - wallMs(from)) / 1000;
}

export function shiftWall(capture, seconds) {
  return wallText(wallMs(capture) + seconds * 1000);
}

// shiftedName replaces the leading "YYYY-MM-DD HH.MM.SS" of name when it is
// the file's old capture time; anything else is left as it is.
export function shiftedName(name, capture, seconds) {
  const stamp = (iso) => iso.slice(0, 10) + ' ' + iso.slice(11, 19).replaceAll(':', '.');
  const old = stamp(wallText(wallMs(capture)));
  if (!name.startsWith(old)) return name;
  return stamp(shiftWall(capture, seconds)) + name.slice(old.length);
}

// folderDateOf is the folder date of a capture time: before dayStartsAt ("HH:MM") it still counts as the day before.
export function folderDateOf(capture, dayStartsAt) {
  const [h, m] = dayStartsAt.split(':').map(Number);
  const ms = wallMs(capture) - (h * 3600 + m * 60) * 1000;
  return wallText(ms).slice(0, 10);
}

// wholeFolderName is the name a folder takes when all its files are shifted
// and now cover `dates`: a dated name takes the earliest, title kept.
export function wholeFolderName(name, dates) {
  const { date, title } = splitName(name);
  if (!date || dates.length === 0) return name;
  return folderName([...dates].sort()[0], title);
}

// isWholeShift is the server's rule (planShift): the shift covers every staged
// file of the folder. The button for "all times" says so; a selection does when
// no kind filter hides files and it holds as many as the folder has.
export function isWholeShift({ button, kind, selected, folderFiles }) {
  return button || (kind === '' && selected === folderFiles);
}

// previewShift works out, for the files that have a capture time, what a shift
// by `seconds` would do. files: [{name, capture_time}]; folder: {name, dates};
// whole: the shift covers every staged file of the folder.
export function previewShift(files, seconds, { dayStartsAt, folder, whole }) {
  const timed = files.filter((f) => f.capture_time).sort((a, b) => (a.capture_time < b.capture_time ? -1 : a.capture_time > b.capture_time ? 1 : 0));
  const skipped = files.length - timed.length;
  const shift = (f) => shiftedName(f.name, f.capture_time, seconds);
  const out = { skipped, count: timed.length, first: null, last: null, moved: null, folderName: null };
  for (const f of timed) {
    const y = Number(shiftWall(f.capture_time, seconds).slice(0, 4));
    if (y < 1970 || y > 2099) {
      out.error = `Shifting by that much would put ${f.name} outside 1970–2099.`;
      return out;
    }
  }
  if (timed.length) {
    out.first = { from: timed[0].name, to: shift(timed[0]), capture: shiftWall(timed[0].capture_time, seconds) };
    const l = timed[timed.length - 1];
    out.last = { from: l.name, to: shift(l), capture: shiftWall(l.capture_time, seconds) };
  }
  if (dayStartsAt && timed.length) {
    const dates = timed.map((f) => folderDateOf(shiftWall(f.capture_time, seconds), dayStartsAt));
    if (whole) out.folderName = wholeFolderName(folder.name, [...new Set(dates)]);
    else out.moved = dates.filter((d) => !folder.dates.includes(d)).length;
  }
  return out;
}
