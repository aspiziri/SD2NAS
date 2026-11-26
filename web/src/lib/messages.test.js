import { test } from 'node:test';
import assert from 'node:assert/strict';
import { plural, joinList, describeError, rescanSummary, promoteSummary, promoteToast, promoteStates, missingCopies, waitingText, copyDetail } from './messages.js';
import { ApiError } from './api.js';

const scan = (o) => ({ adopted_folders: 0, added_files: 0, moved_files: 0, restored_files: 0, culled_files: 0, removed_folders: 0,
  duplicates: 0, busy_folders: 0, errors: [], skipped: '', ...o });

test('plural and joinList', () => {
  assert.equal(plural(1, 'file', 'files'), '1 file');
  assert.equal(plural(1204, 'file', 'files'), '1,204 files');
  assert.equal(joinList(['a']), 'a');
  assert.equal(joinList(['a', 'b']), 'a and b');
  assert.equal(joinList(['a', 'b', 'c']), 'a, b and c');
});

test('describeError keeps server sentences and explains the rest', () => {
  assert.equal(describeError(new ApiError(409, '2025-05-30 is being imported into. Try again when the import finishes.', { code: 'folder_busy' })),
    '2025-05-30 is being imported into. Try again when the import finishes.');
  assert.match(describeError(new ApiError(503, 'catalog: boom')), /^SD2NAS can't do that right now: catalog: boom/);
  assert.match(describeError(new ApiError(0, 'SD2NAS is not reachable: x')), /^SD2NAS is not reachable/);
  assert.equal(describeError(new Error('boom')), 'boom');
});

test('describeError passes every busy holder and the other 4xx sentences through', () => {
  for (const [status, msg, data] of [
    [409, 'X is busy with another change. Try again in a moment.', { code: 'folder_busy', holder: 'operation', folder_id: 3 }],
    [409, 'X is being rescanned. Try again in a moment.', { code: 'folder_busy', holder: 'rescan' }],
    [409, 'X is waiting to be promoted.', { code: 'folder_busy', holder: 'promote', folder_id: 3 }],
    [409, 'X is being promoted.', { code: 'folder_busy', holder: 'promote', folder_id: 3 }],
    [409, 'A folder with that name already exists in Staging. Pick another title.', { code: 'name_taken' }],
    [409, 'Those files changed. Reload and try again.', { code: 'changed' }],
    [409, 'An import is running.', { code: 'ingest_running' }],
    [409, 'Already forgotten.', { code: 'already_forgotten' }],
    [400, 'The title contains "/", which can\'t be used in a folder name.', null],
    [404, 'That folder no longer exists.', null],
  ]) {
    assert.equal(describeError(new ApiError(status, msg, data)), msg);
  }
});

test('describeError falls back to a sentence when the body has none', () => {
  assert.match(describeError(new ApiError(409, '', { code: 'name_taken' })), /already exists in Staging/);
  assert.match(describeError(new ApiError(409, '', { code: 'folder_busy', holder: 'promote' })), /busy/);
  assert.match(describeError(new ApiError(404, '')), /no longer exists/);
  assert.match(describeError(new ApiError(503, '')), /^SD2NAS can't do that right now: /);
  assert.equal(describeError(null), '');
});

test('rescanSummary', () => {
  assert.equal(rescanSummary(scan({})), 'Staging already matched the disk.');
  assert.equal(rescanSummary(scan({ adopted_folders: 1, added_files: 3, culled_files: 1 })),
    'Rescan found 1 new folder, 3 new files and 1 file gone.');
  assert.match(rescanSummary(scan({ busy_folders: 2 })), /2 folders were busy and skipped/);
  assert.match(rescanSummary(scan({ errors: ['x changed while it was read'] })), /1 problem: x changed while it was read/);
  assert.equal(rescanSummary(scan({ skipped: 'Staging/ is missing' })), 'Rescan skipped: Staging/ is missing');
});

test('promoteSummary', () => {
  const folders = [{ state: 'done' }, { state: 'moving' }];
  assert.equal(promoteSummary({ state: 'running', folders, files_done: 10, files_total: 40 }), 'Promoting folder 2 of 2 · 10 of 40 files');
  assert.equal(promoteSummary({ state: 'done', folders: [{ state: 'done' }, { state: 'done' }], files_done: 40, files_total: 40 }),
    'Promoted 2 folders (40 files) to the archive.');
  assert.equal(promoteSummary({ state: 'failed', folders: [{ state: 'done' }, { state: 'failed' }], files_done: 20, files_total: 40 }),
    '1 of 2 folders could not be promoted.');
  assert.equal(promoteSummary({ state: 'cancelled', folders: [{ state: 'waiting' }], files_done: 0, files_total: 5 }), 'Promote cancelled.');
  assert.equal(promoteSummary({ state: 'queued', folders: [{ state: 'waiting' }], files_done: 0, files_total: 5 }), '1 folder is waiting to be promoted.');
});

test('promoteStates marks the folders of the running job and the queue', () => {
  assert.equal(promoteStates(null).size, 0);
  const queue = {
    running: { id: 1, folders: [{ id: 10, state: 'done' }, { id: 11, state: 'moving' }, { id: 12, state: 'waiting' }] },
    queued: [{ id: 2, folders: [{ id: 20, state: 'waiting' }] }],
    last: null,
  };
  const m = promoteStates(queue);
  assert.equal(m.has(10), false); // done: it has left staging
  assert.deepEqual(m.get(11), { job: 1, state: 'moving', cancellable: false });
  assert.deepEqual(m.get(12), { job: 1, state: 'waiting', cancellable: false });
  assert.deepEqual(m.get(20), { job: 2, state: 'waiting', cancellable: true });
});

test('promoteToast', () => {
  const one = { folders: [{}] };
  const two = { folders: [{}, {}] };
  assert.equal(promoteToast(one, { running: null, queued: [], last: null }), 'Promoting 1 folder…');
  assert.equal(promoteToast(two, undefined), 'Promoting 2 folders…');
  assert.equal(promoteToast(one, { running: { id: 1 }, queued: [] }), 'Queued: 1 folder waits for the promote ahead of it.');
  assert.equal(promoteToast(two, { running: null, queued: [{ id: 1 }] }), 'Queued: 2 folders wait for the promote ahead of them.');
});

test('missingCopies', () => {
  assert.equal(missingCopies(1), "1 file's archive copy is missing");
  assert.equal(missingCopies(2), "2 files' archive copies are missing");
  assert.equal(missingCopies(1234), "1,234 files' archive copies are missing");
});

test('waitingText names who holds the folder', () => {
  assert.equal(waitingText('2025-05-30 - Trip', 'promote'), 'Waiting for 2025-05-30 - Trip: a promote is moving it.');
  assert.equal(waitingText('2025-05-30 - Trip', 'operation'), 'Waiting for 2025-05-30 - Trip: a staging change is in progress.');
  assert.equal(waitingText('2025-05-30 - Trip', 'rescan'), 'Waiting for 2025-05-30 - Trip: the staging rescan is checking it.');
  assert.equal(waitingText('2025-05-30 - Trip', 'other'), 'Waiting for 2025-05-30 - Trip: it is busy.');
  assert.equal(waitingText('2025-05-30 - Trip', ''), 'Waiting for 2025-05-30 - Trip: it is busy.');
});

test('copyDetail shows the current big file only', () => {
  assert.equal(copyDetail({ current: 'DCIM/100MSDCF/C0001.MP4', current_size: 8e9, current_done: 2e9 }), 'Copying C0001.MP4 · 2.0 GB of 8.0 GB');
  assert.equal(copyDetail({ current: 'C0001.MP4', current_size: 49_999_999, current_done: 1 }), null);
  assert.equal(copyDetail({ current_size: 8e9, current_done: 1 }), null);
  assert.equal(copyDetail({ current: 'a\\b\\C2.MP4', current_size: 5e7, current_done: 0 }), 'Copying C2.MP4 · 0 B of 50.0 MB');
  assert.equal(copyDetail(null), null);
});
