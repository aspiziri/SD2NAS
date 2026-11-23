import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// `npm run dev` proxies the API to a Go server started with
// `go run ./cmd/sd2nas … --addr :8080` (override with SD2NAS_API). The Host
// header is kept (no changeOrigin), so the request guard's Origin check passes.
const api = process.env.SD2NAS_API || 'http://localhost:8080';

export default defineConfig({
  plugins: [svelte({ compilerOptions: { runes: true } })],
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    emptyOutDir: false, // scripts/clean-dist.mjs empties it but keeps .placeholder
  },
  server: {
    proxy: {
      '/api': { target: api },
      '/healthz': { target: api },
    },
  },
});
