// Fails the build if the emitted console HTML would break the server's CSP
// (`script-src 'self'; style-src 'self'`, no nonce): any inline <script> (one
// without src), any <style> element, any style="" attribute, or an inline event
// handler. Run by `npm run build` after `vite build`; the logic is unit-tested in
// src/test/check-csp.test.ts. The Go side has a matching test over the embedded
// index.html (internal/web/adminui).
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

/** Returns a human-readable description of each CSP violation in `html`. */
export function findInlineViolations(html) {
  const problems = [];
  for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
    if (!/\bsrc\s*=/i.test(m[1])) problems.push(`inline <script>: ${m[2].trim().slice(0, 60)}`);
  }
  if (/<style[\s>]/i.test(html)) problems.push('<style> element');
  // Attribute checks look only inside tags, so prose and comments don't trip them.
  for (const tag of html.matchAll(/<[a-z][^>]*>/gi)) {
    if (/\sstyle\s*=/i.test(tag[0])) problems.push(`style="" attribute: ${tag[0].slice(0, 60)}`);
    if (/\son[a-z]+\s*=/i.test(tag[0]))
      problems.push(`inline event handler: ${tag[0].slice(0, 60)}`);
  }
  return problems;
}

function main() {
  const dist = fileURLToPath(new URL('../../internal/web/adminui/dist', import.meta.url));
  const htmlFiles = readdirSync(dist).filter((f) => f.endsWith('.html'));
  if (!htmlFiles.includes('index.html')) {
    console.error(`check-csp: no index.html in ${dist}; did vite build run?`);
    process.exit(1);
  }
  let failed = false;
  for (const f of htmlFiles) {
    for (const p of findInlineViolations(readFileSync(join(dist, f), 'utf8'))) {
      console.error(`check-csp: ${f}: ${p}`);
      failed = true;
    }
  }
  if (failed) {
    console.error(
      'check-csp: the console must not need inline script/style (see STYLEGUIDE.md, CSP).',
    );
    process.exit(1);
  }
  console.log(`check-csp: ${htmlFiles.length} HTML file(s) clean`);
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) main();
