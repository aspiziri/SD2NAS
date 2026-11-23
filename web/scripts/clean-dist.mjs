// Empties dist/ before a build but keeps dist/.placeholder, which go:embed
// needs on a fresh checkout (vite.config.js sets emptyOutDir: false).
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dist = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist');
mkdirSync(dist, { recursive: true });
for (const name of readdirSync(dist)) {
  if (name !== '.placeholder') rmSync(join(dist, name), { recursive: true, force: true });
}
