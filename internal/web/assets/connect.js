// Connect page: redeem an auth code and show the QR pairing payload.
const form = document.getElementById("redeem-form");
const msg = document.getElementById("msg");
const result = document.getElementById("result");

function showError(text) {
  msg.textContent = text;
  msg.classList.add("show");
}

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  msg.classList.remove("show");
  const code = document.getElementById("code").value.trim();
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
    document.getElementById("qr").src = data.qr_png_data_uri;
    if (data.links) {
      if (data.links.web) setLink("link-web", data.links.web, "Web player");
      if (data.links.ios || data.links.android) {
        setLink("link-apps", data.links.ios || data.links.android, "Mobile apps");
      }
    }
    form.classList.add("hidden");
    result.classList.remove("hidden");
  } catch (err) {
    showError("Network error. Is the server reachable?");
  }
});

function setLink(id, href, label) {
  const el = document.getElementById(id);
  el.href = href;
  el.textContent = label;
  el.removeAttribute("aria-disabled");
}
