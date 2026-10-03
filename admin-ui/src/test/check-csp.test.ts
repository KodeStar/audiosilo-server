import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
// @ts-expect-error - plain .mjs build script without type declarations
import { findInlineViolations } from '../../scripts/check-csp.mjs';

describe('findInlineViolations', () => {
  it('accepts external scripts and stylesheets', () => {
    const html =
      '<head><script src="/admin/theme-init.js"></script><script type="module" crossorigin src="/admin/assets/index.js"></script><link rel="stylesheet" href="/admin/assets/index.css"></head>';
    expect(findInlineViolations(html)).toEqual([]);
  });

  it('flags inline script, style elements, style attributes and handlers', () => {
    const html =
      '<script>alert(1)</script><style>a{}</style><div style="color:red"></div><img onerror="x()">';
    expect(findInlineViolations(html)).toHaveLength(4);
  });

  it('passes the source index.html', () => {
    const html = readFileSync(resolve(process.cwd(), 'index.html'), 'utf8');
    // The dev entry is a module script with src; the theme bootstrap is external.
    expect(findInlineViolations(html)).toEqual([]);
  });
});
