// Generates the admin-console PWA PNG icons from the violet mark in
// internal/web/assets/favicon.svg - the single source of truth for the admin
// brand (keep them in sync; re-run this after editing favicon.svg's colour/mark).
//
// The server has no JS build step, so the generated PNGs are committed. To
// regenerate, run with a Node that can resolve `sharp`. In this workspace the
// sibling frontend already has it, so from anywhere:
//   node scripts/gen-admin-icons.mjs
// resolves sharp from ../../audiosilo-frontend/node_modules as a fallback.

import { readFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const assets = resolve(here, '../internal/web/assets');

// Resolve sharp: prefer a normal install, fall back to the sibling frontend's
// (the documented workspace layout) so this works without adding a server dep.
let sharp;
try {
  sharp = (await import('sharp')).default;
} catch {
  const sibling = resolve(here, '../../audiosilo-frontend/node_modules/sharp/dist/index.mjs');
  sharp = (await import(new URL(`file://${sibling}`))).default;
}

// Pull the mark out of favicon.svg: inner viewBox, path, fill, and rotate.
const svg = await readFile(resolve(assets, 'favicon.svg'), 'utf8');
const viewBox = /viewBox="([^"]+)"\s+preserveAspectRatio/.exec(svg)[1];
const path = /<path d="([^"]+)"/.exec(svg)[1];
const fill = /<path[^>]*fill="([^"]+)"/.exec(svg)[1];
const transform = /transform="([^"]+)"/.exec(svg)[1];

// A square master: solid background (PWA icons read best opaque; maskable needs
// it) + the mark centred with `pad` fraction of breathing room.
function master({ size, pad, bg = '#ffffff' }) {
  const inset = Math.round(size * pad);
  const box = size - inset * 2;
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" viewBox="0 0 ${size} ${size}">` +
    `<rect width="${size}" height="${size}" fill="${bg}"/>` +
    `<svg x="${inset}" y="${inset}" width="${box}" height="${box}" viewBox="${viewBox}" preserveAspectRatio="xMidYMid meet">` +
    `<path d="${path}" fill="${fill}" transform="${transform}"/>` +
    `</svg></svg>`
  );
}

async function emit(name, opts) {
  await sharp(Buffer.from(master(opts))).png().toFile(resolve(assets, name));
  console.log('png', name, `${opts.size}²`);
}

await emit('icon-192.png', { size: 192, pad: 0.12 }); // any
await emit('icon-512.png', { size: 512, pad: 0.12 }); // any
await emit('icon-512-maskable.png', { size: 512, pad: 0.22 }); // safe-zone padding
console.log('done');
