// First-run setup wizard. Posts to /setup with the one-time setup token, which
// rides in the URL fragment (#token=...) so it never reaches the server's logs.
// All behaviour is external + addEventListener to satisfy the strict CSP.
const $ = (id) => document.getElementById(id);

function setupToken() {
  const h = new URLSearchParams(location.hash.slice(1));
  return h.get("token") || "";
}

function showError(msg) {
  const m = $("setup-msg");
  m.textContent = msg;
  m.classList.add("show");
}

$("setup-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("setup-msg").classList.remove("show");
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
        library_name: $("library-name").value.trim(),
        library_root: $("library-root").value.trim(),
      }),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(data.error || "Setup failed.");
    $("setup-form").classList.add("hidden");
    $("setup-done").classList.remove("hidden");
  } catch (err) {
    showError(err.message);
    btn.disabled = false;
  }
});
