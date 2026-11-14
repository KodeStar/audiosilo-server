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

// redeem asks the server for the code's pairing. fromLink: the code came in an
// invite link's fragment, not typed into the form, which the page's "open this
// invite link again" wording assumes.
async function redeem(code, fromLink) {
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
    // The QR encodes data.web_url: the app's own scanner (Scan a QR code) pairs
    // the app with it; a phone's camera opens the embedded web player, which
    // exchanges the pairing token. A token redeemed from an invite honors the
    // invite's uses/expiry, so each device being set up can scan the same QR;
    // uses_remaining reports that budget (absent = unlimited or not
    // invite-derived).
    document.getElementById("qr").src = data.qr_png_data_uri;
    if (data.uses_remaining != null) {
      const note = document.getElementById("uses-note");
      note.textContent = asI18n.t("connect.usesRemaining", { count: data.uses_remaining });
      note.classList.remove("hidden");
    }
    if (data.uri) setLink("link-app", data.uri, asI18n.t("connect.openApp"));
    if (data.web_url) setLink("link-web", data.web_url, asI18n.t("connect.openWeb"));
    // The console only for an admin's invite (a listener has no use for it).
    document.getElementById("link-admin").classList.toggle("hidden", data.admin !== true);
    // Typed in: there is no link to open again, only this page to come back to.
    if (!fromLink) {
      retext("phone-heading", "connect.phoneHeadingTyped");
      retext("phone-body", "connect.phoneBodyTyped");
      retext("reopen-hint", "connect.reopenHintTyped");
    }
    if (isPhone()) showPhone();
    form.classList.add("hidden");
    document.getElementById("intro").classList.add("hidden");
    result.classList.remove("hidden");
  } catch (err) {
    showError(asI18n.t("connect.networkError"));
  }
}

form.addEventListener("submit", (e) => {
  e.preventDefault();
  redeem(codeInput.value.trim(), false);
});

// retext swaps an element's text for another key's, kept on data-i18n so a
// language change keeps it.
function retext(id, key) {
  const el = document.getElementById(id);
  el.setAttribute("data-i18n", key);
  el.textContent = asI18n.t(key);
}

// isPhone: a phone or tablet, the device the app would run on. A QR shown there
// can't be scanned by the phone holding it, so the page leads with getting the
// app instead.
function isPhone() {
  const ua = navigator.userAgentData;
  if (ua && ua.mobile === true) return true;
  if (/Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent)) return true;
  // iPadOS reports a Mac; a touch screen tells it apart.
  return navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1;
}

// showPhone lays the result out for the phone holding it: "get the app, then
// come back to this link" with the store links under it, and the web player
// first; no self-scan QR.
function showPhone() {
  const lead = document.getElementById("phone-lead");
  lead.classList.remove("hidden");
  lead.after(document.getElementById("get-app"));
  document.getElementById("scan-block").classList.add("hidden");
  document.getElementById("reopen-hint").classList.add("hidden");
  const web = document.getElementById("link-web");
  web.parentElement.prepend(web);
}

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
  redeem(code, true);
})();
