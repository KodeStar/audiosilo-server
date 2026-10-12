// Applies the theme to the connect and setup pages before first paint, as the
// admin console's admin-ui/public/theme-init.js does (and reading the same storage
// key, so a theme chosen in the console carries over). It is an external file (not
// an inline <script>) because these pages' CSP is `script-src 'self'`. It resolves
// the stored preference - light, dark or system - to an explicit data-theme on
// <html>, which is what style.css's tokens key on.
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
