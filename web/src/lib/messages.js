import { formatCount } from './format.js';

export const plural = (n, one, many) => `${formatCount(n)} ${n === 1 ? one : many}`;

export function joinList(parts) {
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts.at(-1)}`;
}

// Used only when an error body carries no sentence of its own.
const BY_CODE = {
  folder_busy: 'That folder is busy. Try again in a moment.',
  name_taken: 'A folder with that name already exists in Staging. Pick another title.',
  changed: 'Staging changed while you were looking. The list has been refreshed; try again.',
  ingest_running: 'An import is running. Try again when it finishes.',
  already_forgotten: 'That import has already been forgotten.',
  promote_started: "That promote has already started or finished, so it can't be cancelled.",
};
const BY_STATUS = {
  400: 'SD2NAS rejected that request.',
  404: 'That no longer exists.',
};

// The server already writes whole sentences for 4xx answers (409s name the
// folder and say why: folder_busy for every holder, name_taken, changed,
// ingest_running, already_forgotten, and the 400/404 reasons), so those are
// shown as they are; only unavailability gets a lead-in, and a body without
// a sentence falls back to one per code or status.
export function describeError(err) {
  if (!err) return '';
  const msg = err.message || '';
  if (err.status === 503) return `SD2NAS can't do that right now: ${msg || 'it is shutting down'}`;
  if (msg) return msg;
  return BY_CODE[err.data?.code] || BY_STATUS[err.status] || String(err);
}

export function rescanSummary(r) {
  if (r.skipped) return `Rescan skipped: ${r.skipped}`;
  const found = [];
  if (r.adopted_folders) found.push(plural(r.adopted_folders, 'new folder', 'new folders'));
  if (r.added_files) found.push(plural(r.added_files, 'new file', 'new files'));
  if (r.moved_files) found.push(`${plural(r.moved_files, 'file', 'files')} moved`);
  if (r.restored_files) found.push(`${plural(r.restored_files, 'culled file', 'culled files')} back`);
  if (r.culled_files) found.push(`${plural(r.culled_files, 'file', 'files')} gone`);
  if (r.removed_folders) found.push(`${plural(r.removed_folders, 'folder', 'folders')} gone`);
  let s = found.length ? `Rescan found ${joinList(found)}.` : 'Staging already matched the disk.';
  if (r.busy_folders) s += ` ${plural(r.busy_folders, 'folder was', 'folders were')} busy and skipped.`;
  if (r.errors?.length) s += ` ${plural(r.errors.length, 'problem', 'problems')}: ${r.errors[0]}`;
  return s;
}

// promoteStates maps the id of every folder still waiting or moving in the
// running job or the queue to its job; only a queued job can be cancelled.
export function promoteStates(queue) {
  const out = new Map();
  if (!queue) return out;
  if (queue.running) {
    for (const f of queue.running.folders) {
      if (f.state === 'waiting' || f.state === 'moving') out.set(f.id, { job: queue.running.id, state: f.state, cancellable: false });
    }
  }
  for (const job of queue.queued) {
    for (const f of job.folders) out.set(f.id, { job: job.id, state: 'waiting', cancellable: true });
  }
  return out;
}

export function promoteSummary(job) {
  const n = job.folders.length;
  if (job.state === 'queued') return `${plural(n, 'folder is', 'folders are')} waiting to be promoted.`;
  if (job.state === 'cancelled') return 'Promote cancelled.';
  if (job.state === 'running') {
    const at = Math.min(n, job.folders.filter((f) => f.state === 'done' || f.state === 'failed').length + 1);
    return `Promoting folder ${at} of ${n} · ${formatCount(job.files_done)} of ${formatCount(job.files_total)} files`;
  }
  const failed = job.folders.filter((f) => f.state === 'failed').length;
  if (failed) return `${failed} of ${plural(n, 'folder', 'folders')} could not be promoted.`;
  return `Promoted ${plural(n, 'folder', 'folders')} (${plural(job.files_done, 'file', 'files')}) to the archive.`;
}

// The toast after a promote is enqueued. `before` is the queue as the page saw it
// just before the request ({running, queued}); the response always says "queued"
// (the worker has not started it yet), so only a job already running or waiting
// ahead of it means a wait.
export function promoteToast(job, before) {
  const n = job.folders.length;
  if (before?.running || before?.queued?.length) {
    return `Queued: ${plural(n, 'folder waits', 'folders wait')} for the promote ahead of ${n === 1 ? 'it' : 'them'}.`;
  }
  return `Promoting ${plural(n, 'folder', 'folders')}…`;
}
