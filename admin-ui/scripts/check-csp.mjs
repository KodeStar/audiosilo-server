// Fails the build if the emitted console HTML would break the server's CSP
// (`script-src 'self'; style-src 'self'`, no nonce): any inline <script> (one
// without src), any <style> element, any style="" attribute, or an inline event
// handler. Wired into vite.config.ts as a plugin; the logic is unit-tested in
// src/test/check-csp.test.ts. The Go side has a matching test over the embedded
// index.html (internal/web/adminui).
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

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

/**
 * Vite plugin: after the bundle is written, fail the build if any emitted HTML
 * file needs inline script/style. Runs on every `vite build`, so it can't be
 * skipped by building without the npm script.
 */
export function cspCheck() {
  let outDir = '';
  return {
    name: 'csp-check',
    apply: 'build',
    configResolved(config) {
      outDir = config.build.outDir;
    },
    closeBundle() {
      const htmlFiles = readdirSync(outDir).filter((f) => f.endsWith('.html'));
      if (!htmlFiles.includes('index.html'))
        throw new Error(`csp-check: no index.html in ${outDir}`);
      const problems = htmlFiles.flatMap((f) =>
        findInlineViolations(readFileSync(join(outDir, f), 'utf8')).map((p) => `${f}: ${p}`),
      );
      if (problems.length) {
        throw new Error(
          `csp-check: the console must not need inline script/style (STYLEGUIDE.md section 13):\n${problems.join('\n')}`,
        );
      }
    },
  };
}
