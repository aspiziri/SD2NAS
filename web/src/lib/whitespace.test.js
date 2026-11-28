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
// start or end of any fragment (an element's content, an {#if} or {#each} body,
// a component's children) is removed, so `Archive<span> index</span>` renders
// "Archiveindex". Line breaks are layout, so only same-line spaces count.
function trimmed(file) {
  const found = [];
  const walk = (node, where) => {
    if (!node || typeof node !== 'object') return;
    if (Array.isArray(node)) return node.forEach((n) => walk(n, where));
    if (node.type === 'RegularElement') {
      if (['pre', 'textarea'].includes(node.name)) return;
      where = `<${node.name}>`;
    } else if (node.type === 'Component') {
      where = `<${node.name}>`;
    } else if (node.type?.endsWith('Block')) {
      where = node.type;
    }
    if (node.type === 'Fragment') {
      const first = node.nodes[0], last = node.nodes[node.nodes.length - 1];
      if (first?.type === 'Text' && /^[ \t]+\S/.test(first.data)) found.push(`${where} starts ${JSON.stringify(first.data)}`);
      if (last?.type === 'Text' && /\S[ \t]+$/.test(last.data)) found.push(`${where} ends ${JSON.stringify(last.data)}`);
    }
    for (const [k, v] of Object.entries(node)) if (k !== 'metadata') walk(v, where);
  };
  walk(parse(readFileSync(file, 'utf8'), { modern: true }).fragment, 'top level');
  return found;
}

test('no fragment starts or ends its text with a space Svelte would drop', () => {
  const files = svelteFiles(src);
  assert.ok(files.length > 0);
  const bad = files.flatMap((f) => trimmed(f).map((t) => `${relative(src, f)}: ${t}`));
  assert.deepEqual(bad, [], 'write the space as {\' \'} or move it outside the element or block');
});
