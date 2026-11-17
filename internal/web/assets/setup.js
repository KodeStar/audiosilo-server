// First-run setup wizard: creates the admin account only, then hands over to the
// admin console's setup flow. Posts to /setup with the one-time setup token, which
// rides in the URL fragment (#token=...) so it never reaches the server's logs.
// All behaviour is external + addEventListener to satisfy the strict CSP.
const $ = (id) => document.getElementById(id);

function setupToken() {
  const h = new URLSearchParams(location.hash.slice(1));
  return h.get("token") || "";
}

// The server's minimum (auth.MinPasswordLen); the field's minlength says the same.
const MIN_PASSWORD = 8;

function showError(msg) {
  const m = $("setup-msg");
  m.textContent = msg;
  m.classList.add("show");
}

// A server refusal as a sentence: its own words start lowercase
// ("password must be at least 8 characters").
function sentence(msg) {
  const s = msg.trim();
  if (!s) return "Setup failed.";
  const first = s.charAt(0).toUpperCase() + s.slice(1);
  return /[.!?]$/.test(first) ? first : first + ".";
}

$("setup-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("setup-msg").classList.remove("show");
  if ($("password").value.length < MIN_PASSWORD) {
    showError(`Choose a password of at least ${MIN_PASSWORD} characters.`);
    return;
  }
  const btn = $("setup-submit");
  btn.disabled = true;
  try {
    const resp = await fetch("/setup", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        token: setupToken(),
        username: $("username").value.trim(),
        password: $("password").value,
      }),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(sentence(data.error || ""));
    $("setup-form").classList.add("hidden");
    $("setup-done").classList.remove("hidden");
  } catch (err) {
    showError(err.message);
    btn.disabled = false;
  }
});
