// Connect page: redeem an auth code and show the QR pairing payload. The
// copy-invite link drops the recipient here with the code in the URL fragment, so
// we auto-fill and submit when one is present.
const form = document.getElementById("redeem-form");
const msg = document.getElementById("msg");
const result = document.getElementById("result");
const codeInput = document.getElementById("code");

function showError(text) {
  msg.textContent = text;
  msg.classList.add("show");
}

async function redeem(code) {
  if (!code) return;
  msg.classList.remove("show");
  try {
    const resp = await fetch("/api/v1/auth/redeem", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code }),
    });
    const data = await resp.json();
    if (!resp.ok) {
      showError(data.error || asI18n.t("connect.redeemError"));
      return;
    }
    // The QR encodes data.web_url: scanning opens the app (when it claims this
    // domain) or the embedded web player, which exchanges the pairing token. A
    // token redeemed from an invite honors the invite's uses/expiry, so each
    // device being set up can scan the same QR; uses_remaining reports that
    // budget (absent = unlimited or not invite-derived).
    document.getElementById("qr").src = data.qr_png_data_uri;
    if (data.uses_remaining != null) {
      const note = document.getElementById("uses-note");
      note.textContent = asI18n.t("connect.usesRemaining", { n: data.uses_remaining });
      note.classList.remove("hidden");
    }
    if (data.uri) setLink("link-app", data.uri, asI18n.t("connect.openApp"));
    if (data.web_url) setLink("link-web", data.web_url, asI18n.t("connect.openWeb"));
    form.classList.add("hidden");
    result.classList.remove("hidden");
  } catch (err) {
    showError(asI18n.t("connect.networkError"));
  }
}

form.addEventListener("submit", (e) => {
  e.preventDefault();
  redeem(codeInput.value.trim());
});

function setLink(id, href, label) {
  const el = document.getElementById(id);
  el.href = href;
  el.textContent = label;
  el.removeAttribute("aria-disabled");
}

// Auto-redeem from a copy-invite link (#code=XXXX-...). Strip the fragment from
// the URL bar afterwards so the code isn't bookmarked or shoulder-surfed.
(function () {
  const m = /(?:^|[#&])code=([^&]+)/.exec(location.hash);
  if (!m) return;
  const code = decodeURIComponent(m[1]).trim();
  history.replaceState(null, "", location.pathname + location.search);
  codeInput.value = code;
  redeem(code);
})();
