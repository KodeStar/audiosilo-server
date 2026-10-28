// Connect page: redeem an invite code and show the QR pairing payload. The
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

// redeemFailure is the sentence for a refused redeem, in the page's language:
// by status and the server's error code, never the server's English message.
function redeemFailure(resp, data) {
  if (resp.status === 429) {
    const secs = parseInt(resp.headers.get("Retry-After") || "", 10);
    const mins = Math.max(1, Math.ceil((secs > 0 ? secs : 900) / 60));
    return asI18n.t("connect.tooMany", { count: mins });
  }
  if (resp.status === 401) {
    if (data && data.code === "invite_used") return asI18n.t("connect.inviteUsed");
    if (data && data.code === "invite_expired") return asI18n.t("connect.inviteExpired");
    return asI18n.t("connect.invalidCode");
  }
  if (resp.status === 400) return asI18n.t("connect.codeRequired");
  return asI18n.t("connect.redeemError");
}

async function redeem(code) {
  msg.classList.remove("show");
  if (!code) {
    showError(asI18n.t("connect.codeRequired"));
    return;
  }
  try {
    const resp = await fetch("/api/v1/auth/redeem", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code }),
    });
    const data = await resp.json().catch(() => null);
    if (!resp.ok || !data) {
      showError(redeemFailure(resp, data));
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
      note.textContent = asI18n.t("connect.usesRemaining", { count: data.uses_remaining });
      note.classList.remove("hidden");
    }
    if (data.uri) setLink("link-app", data.uri, asI18n.t("connect.openApp"));
    if (data.web_url) setLink("link-web", data.web_url, asI18n.t("connect.openWeb"));
    form.classList.add("hidden");
    document.getElementById("intro").classList.add("hidden");
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
