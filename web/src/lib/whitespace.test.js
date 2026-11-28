import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'svelte/compiler';

const src = fileURLToPath(new URL('..', import.meta.url));

function svelteFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? svelteFiles(join(dir, e.name)) : e.name.endsWith('.svelte') ? [join(dir, e.name)] : []);
}

// trimmed lists the text Svelte 5 would drop spaces from: a space at the very
// start or end of an element's content is removed, so `Archive<span> index</span>`
// renders "Archiveindex". Line breaks are layout, so only same-line spaces count.
function trimmed(file) {
  const found = [];
  const walk = (node) => {
    if (!node || typeof node !== 'object') return;
    if (Array.isArray(node)) return node.forEach(walk);
    if (node.type === 'RegularElement' && !['pre', 'textarea'].includes(node.name)) {
      const nodes = node.fragment.nodes;
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (first?.type === 'Text' && /^[ \t]+\S/.test(first.data)) found.push(`<${node.name}>${first.data}`);
      if (last?.type === 'Text' && /\S[ \t]+$/.test(last.data)) found.push(`${last.data}</${node.name}>`);
    }
    for (const [k, v] of Object.entries(node)) if (k !== 'metadata') walk(v);
  };
  walk(parse(readFileSync(file, 'utf8'), { modern: true }).fragment);
  return found;
}

test('no element starts or ends its text with a space Svelte would drop', () => {
  const files = svelteFiles(src);
  assert.ok(files.length > 0);
  const bad = files.flatMap((f) => trimmed(f).map((t) => `${relative(src, f)}: ${JSON.stringify(t)}`));
  assert.deepEqual(bad, [], 'write the space as {\' \'} or move it outside the element');
});
