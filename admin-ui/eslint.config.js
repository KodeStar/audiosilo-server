import js from '@eslint/js';
import { defineConfig } from 'eslint/config';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import tseslint from 'typescript-eslint';

// The console is served under `script-src 'self'; style-src 'self'` with no nonce.
// These rules stop the usual ways a dependency or component smuggles in an inline
// <style>/<script> (see STYLEGUIDE.md and ADMIN-CONSOLE-PLAN.md decision 3).
const cspRestrictedImports = {
  patterns: [
    { group: ['@radix-ui/*'], message: 'Radix injects inline styles the CSP blocks; use Base UI.' },
  ],
  paths: [
    { name: 'sonner', message: 'sonner injects a <style>; use Base UI Toast (src/lib/toast.ts).' },
    { name: 'vaul', message: 'vaul is Radix-based; use the Base UI Drawer.' },
    { name: 'next-themes', message: 'next-themes injects an inline script; use src/lib/theme.ts.' },
    {
      name: 'echarts',
      message: 'ECharts injects styles; use Recharts via the shadcn chart (minus ChartStyle).',
    },
    {
      name: 'zod',
      message: "Import z from '@/lib/zod': it sets jitless, or zod's eval probe trips the CSP.",
    },
  ],
};

const cspRestrictedSyntax = [
  {
    selector: "JSXOpeningElement[name.name='style']",
    message: 'No <style> elements: the CSP blocks them. Put CSS in globals.css.',
  },
  {
    selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
    message:
      'No dangerouslySetInnerHTML: render React elements (inline markup can carry blocked style/script).',
  },
  {
    selector: "JSXMemberExpression[object.name='Command'][property.name='Dialog']",
    message:
      "cmdk's Command.Dialog is Radix-based and injects a <style>; wrap <Command> in Base UI Dialog.",
  },
  {
    selector: "JSXIdentifier[name='ChartStyle']",
    message: "shadcn's ChartStyle renders a <style>; put chart colours in globals.css.",
  },
];

// A grid whose columns come only from a min-width breakpoint (`md:grid-cols-2`) is
// one implicit `auto` column on a phone, which grows to the width of any truncated
// line inside it and overflows the page. Flag a class string with a breakpoint
// column template and no base `grid-cols-*` of its own. Breakpoints are the named
// ones, arbitrary `min-[...]` and container `@md`, with any variants stacked after
// (`md:hover:`); `max-md:` is not a min-width breakpoint, so it is left alone. The
// base must sit in the same string as the breakpoint template; a template literal
// counts as one string, its quasis and any string literal inside it alike.
const breakpointCols =
  '/(^|[\\s:])(sm|md|lg|xl|2xl|min-\\[[^\\]]*\\]|@[a-z0-9]+):([^\\s:]+:)*grid-cols-/';
const baseCols = '/(^|\\s)grid-cols-/';
const gridMessage =
  'A breakpoint-only grid needs a base grid-cols-1 in the same class string (STYLEGUIDE.md section 12: responsive grid needs a base grid-cols-1).';
const layoutRestrictedSyntax = [
  {
    selector: `Literal[value=${breakpointCols}]:not([value=${baseCols}]):not(TemplateLiteral:has(> TemplateElement[value.cooked=${baseCols}]) Literal)`,
    message: gridMessage,
  },
  {
    selector: `TemplateLiteral:has(> TemplateElement[value.cooked=${breakpointCols}]):not(:has(> TemplateElement[value.cooked=${baseCols}]))`,
    message: gridMessage,
  },
];

export default defineConfig(
  {
    ignores: ['dist', 'node_modules', 'coverage'],
  },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, tseslint.configs.recommended, reactRefresh.configs.vite],
    // The classic hooks rules only. eslint-plugin-react-hooks 7's `recommended` adds the
    // React Compiler rules (purity, refs, set-state-in-effect, ...); adopting them is a
    // separate change.
    plugins: { 'react-hooks': reactHooks },
    languageOptions: {
      ecmaVersion: 2022,
      globals: {
        ...globals.browser,
      },
    },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      'no-restricted-imports': ['error', cspRestrictedImports],
      'no-restricted-syntax': ['error', ...cspRestrictedSyntax, ...layoutRestrictedSyntax],
    },
  },
  {
    // The one place zod is imported (and configured).
    files: ['src/lib/zod.ts'],
    rules: { 'no-restricted-imports': ['error', { patterns: cspRestrictedImports.patterns }] },
  },
  {
    files: ['**/*.test.{ts,tsx}', 'src/test/**/*.{ts,tsx}'],
    languageOptions: {
      globals: {
        ...globals.node,
      },
    },
  },
  {
    files: ['vite.config.ts', 'scripts/**/*.mjs'],
    extends: [js.configs.recommended],
    languageOptions: {
      globals: {
        ...globals.node,
      },
    },
  },
);
