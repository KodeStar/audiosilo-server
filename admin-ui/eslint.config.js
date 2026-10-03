import js from '@eslint/js';
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

export default tseslint.config(
  {
    ignores: ['dist', 'node_modules', 'coverage'],
  },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      ...tseslint.configs.recommended,
      reactHooks.configs['recommended-latest'],
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2022,
      globals: {
        ...globals.browser,
      },
    },
    rules: {
      'no-restricted-imports': ['error', cspRestrictedImports],
      'no-restricted-syntax': ['error', ...cspRestrictedSyntax],
    },
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
