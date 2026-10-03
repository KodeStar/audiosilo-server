// Applies the admin console's theme before first paint. It is an external file
// (not an inline <script>) because the console's CSP is `script-src 'self'` with
// no nonce. It resolves the stored preference - light, dark or system - to an
// explicit data-theme on <html>, which is what both the CSS tokens and Tailwind's
// `dark:` variant key on. Keep it in sync with src/lib/theme.ts (same storage key).
(function () {
  var pref = 'system';
  try {
    pref = localStorage.getItem('audiosilo.admin.theme') || 'system';
  } catch (e) {
    /* storage unavailable (private mode): fall back to the system preference */
  }
  var dark =
    pref === 'dark' ||
    (pref !== 'light' && window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
})();
