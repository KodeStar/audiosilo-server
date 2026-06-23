// Tiny i18n engine for the baked-in admin/connect UI. No build step, no deps.
//
// The dictionary is a plain object on `window.asI18nDict` (see i18n-dict.js), loaded
// synchronously BEFORE this script and before the page script — so `asI18n.t()` is
// ready when admin.js/connect.js render dynamic content, with no fetch race or flash.
// English is the base + fallback; other languages may be partial.
//
// Static markup is tagged with data-i18n / data-i18n-placeholder / data-i18n-title /
// data-i18n-aria and translated by apply(); dynamic strings call asI18n.t(key, vars).
// Loaded as an external script so the strict CSP (script-src 'self') is unaffected.
(function () {
  "use strict";
  var SUPPORTED = ["en", "es", "fr", "de", "pt", "it"];
  var LABELS = { en: "English", es: "Español", fr: "Français", de: "Deutsch", pt: "Português", it: "Italiano" };
  var STORE_KEY = "audiosilo.lang";
  var DICT = window.asI18nDict || {};
  var BASE = DICT.en || {};

  function detect() {
    try {
      var saved = localStorage.getItem(STORE_KEY);
      if (saved && SUPPORTED.indexOf(saved) >= 0) return saved;
    } catch (e) {
      /* localStorage unavailable (private mode) — fall through to navigator */
    }
    var langs = navigator.languages || [navigator.language || "en"];
    for (var i = 0; i < langs.length; i++) {
      var code = String(langs[i] || "").slice(0, 2).toLowerCase();
      if (SUPPORTED.indexOf(code) >= 0) return code;
    }
    return "en";
  }

  var current = detect();

  function table() {
    return DICT[current] || BASE;
  }

  function t(key, vars) {
    var active = table();
    var s = active[key] != null ? active[key] : BASE[key] != null ? BASE[key] : key;
    if (vars) {
      s = s.replace(/\{\{(\w+)\}\}/g, function (_, k) {
        return vars[k] != null ? String(vars[k]) : "";
      });
    }
    return s;
  }

  function apply(root) {
    root = root || document;
    root.querySelectorAll("[data-i18n]").forEach(function (el) {
      el.textContent = t(el.getAttribute("data-i18n"));
    });
    root.querySelectorAll("[data-i18n-placeholder]").forEach(function (el) {
      el.setAttribute("placeholder", t(el.getAttribute("data-i18n-placeholder")));
    });
    root.querySelectorAll("[data-i18n-title]").forEach(function (el) {
      el.setAttribute("title", t(el.getAttribute("data-i18n-title")));
    });
    root.querySelectorAll("[data-i18n-alt]").forEach(function (el) {
      el.setAttribute("alt", t(el.getAttribute("data-i18n-alt")));
    });
    root.querySelectorAll("[data-i18n-aria]").forEach(function (el) {
      el.setAttribute("aria-label", t(el.getAttribute("data-i18n-aria")));
    });
    document.documentElement.lang = current;
  }

  function buildSwitchers() {
    document.querySelectorAll("select[data-i18n-switch]").forEach(function (sel) {
      if (!sel.dataset.built) {
        sel.dataset.built = "1";
        SUPPORTED.forEach(function (l) {
          var o = document.createElement("option");
          o.value = l;
          o.textContent = LABELS[l];
          sel.appendChild(o);
        });
        sel.addEventListener("change", function () {
          setLang(sel.value);
        });
      }
      sel.value = current;
    });
  }

  function setLang(lang) {
    if (SUPPORTED.indexOf(lang) < 0) return;
    try {
      localStorage.setItem(STORE_KEY, lang);
    } catch (e) {
      /* ignore persistence failure */
    }
    current = lang;
    apply();
    buildSwitchers();
    document.dispatchEvent(new CustomEvent("as-lang-changed", { detail: { lang: lang } }));
  }

  window.asI18n = {
    t: t,
    apply: apply,
    setLang: setLang,
    get lang() {
      return current;
    },
  };

  function init() {
    apply();
    buildSwitchers();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
