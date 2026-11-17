// Applies the admin console's theme before first paint. It is an external file
// (not an inline <script>) because the console's CSP is `script-src 'self'` with
// no nonce. It resolves the stored preference - light, dark or system - to an
// explicit data-theme on <html>, which is what both the CSS tokens and Tailwind's
// `dark:` variant key on. Keep it in sync with src/lib/theme.ts (same storage key).
(function () {
  var mq = window.matchMedia && matchMedia('(prefers-color-scheme: dark)');
  function apply() {
    var pref = 'system';
    try {
      pref = localStorage.getItem('audiosilo.admin.theme') || 'system';
    } catch (e) {
      /* storage unavailable (private mode): fall back to the system preference */
    }
    var dark = pref === 'dark' || (pref !== 'light' && !!mq && mq.matches);
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  }
  apply();
  // Follow the OS while the preference is "system": the stored preference is
  // read again on each change, so a choice made since the page loaded holds.
  if (mq && mq.addEventListener) mq.addEventListener('change', apply);
  else if (mq && mq.addListener) mq.addListener(apply);
})();
