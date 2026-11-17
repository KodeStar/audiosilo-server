// Applies the theme to the connect and setup pages before first paint, as the
// admin console's admin-ui/public/theme-init.js does (and reading the same storage
// key, so a theme chosen in the console carries over). It is an external file (not
// an inline <script>) because these pages' CSP is `script-src 'self'`. It resolves
// the stored preference - light, dark or system - to an explicit data-theme on
// <html>, which is what style.css's tokens key on, and keeps it in step when the
// OS switches between light and dark while the page is open.
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
