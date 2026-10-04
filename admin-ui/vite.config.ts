/// <reference types="vitest/config" />
import { writeFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';
import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
// @ts-expect-error - plain .mjs build script without type declarations
import { cspCheck } from './scripts/check-csp.mjs';

// The Go server embeds this directory (internal/web/adminui, //go:embed all:dist).
// Only its .gitkeep is committed; the build output is gitignored and produced by
// CI, the Dockerfile and GoReleaser.
const outDir = fileURLToPath(new URL('../internal/web/adminui/dist', import.meta.url));

// emptyOutDir wipes the committed .gitkeep; put it back so `git status` stays clean
// and a later source build without Node still compiles the embed.
const keepGitkeep: Plugin = {
  name: 'keep-gitkeep',
  apply: 'build',
  closeBundle() {
    writeFileSync(`${outDir}/.gitkeep`, '');
  },
};

// Dev loop: run the Go server on :8080 (AUDIOSILO_TLS_MODE=off), then `npm run dev`
// and open http://localhost:5173/admin/. API calls and the server's own files
// (favicon, icons, manifest, service worker) are proxied to Go. The dev server
// isn't under the production CSP.
const goServer = process.env.AUDIOSILO_DEV_SERVER ?? 'http://127.0.0.1:8080';

export default defineConfig({
  base: '/admin/',
  plugins: [react(), tailwindcss(), keepGitkeep, cspCheck()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir,
    emptyOutDir: true,
    // Never inline assets as data: URIs into JS/CSS; every font and image is a
    // same-origin file (the CSP has no font-src data:).
    assetsInlineLimit: 0,
    // The shell + Overview load up front (~280 KB gzip: the entry chunk plus the shared
    // chunks it preloads), cached immutably; each other screen is a lazy chunk
    // (src/features/section-page.tsx).
    chunkSizeWarningLimit: 1024,
  },
  server: {
    proxy: {
      '/api': goServer,
      '/assets': goServer,
      '/manifest.webmanifest': goServer,
      '/sw.js': goServer,
      '/web': goServer,
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
});
