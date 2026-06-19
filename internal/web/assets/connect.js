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
      showError(data.error || "Could not redeem auth code.");
      return;
    }
    // The QR encodes data.web_url: scanning opens the app (when it claims this
    // domain) or the embedded web player, which exchanges the single-use token.
    document.getElementById("qr").src = data.qr_png_data_uri;
    if (data.uri) setLink("link-app", data.uri, "Open in app");
    if (data.web_url) setLink("link-web", data.web_url, "Open web player");
    form.classList.add("hidden");
    result.classList.remove("hidden");
  } catch (err) {
    showError("Network error. Is the server reachable?");
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
