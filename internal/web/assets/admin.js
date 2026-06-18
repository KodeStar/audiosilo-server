// Admin console. A static client over the JSON API; the server enforces the
// admin role on every /admin/* call, so this file holds no privileged logic.
const TOKEN_KEY = "audiosilo_token";
let token = localStorage.getItem(TOKEN_KEY);

const el = (id) => document.getElementById(id);
const loginView = el("login-view");
const dashView = el("dash-view");

// api wraps fetch with auth + JSON handling. On 401 it forces re-login.
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (token) opts.headers["Authorization"] = "Bearer " + token;
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch("/api/v1" + path, opts);
  if (resp.status === 401) {
    logout();
    throw new Error("Session expired. Please sign in again.");
  }
  const text = await resp.text();
  const data = text ? JSON.parse(text) : {};
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

function flash(text, kind) {
  const m = el("global-msg");
  m.textContent = text;
  m.className = "msg show " + (kind || "ok");
  if (kind !== "error") setTimeout(() => m.classList.remove("show"), 4000);
}

function logout() {
  token = null;
  localStorage.removeItem(TOKEN_KEY);
  dashView.classList.add("hidden");
  loginView.classList.remove("hidden");
}

// ---- Login ----
el("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const msg = el("login-msg");
  msg.classList.remove("show");
  try {
    const data = await postLogin(el("username").value, el("password").value);
    if (data.user.role !== "admin") {
      msg.textContent = "This account is not an administrator.";
      msg.classList.add("show");
      return;
    }
    token = data.token;
    localStorage.setItem(TOKEN_KEY, token);
    el("who").textContent = data.user.username;
    enterDashboard();
  } catch (err) {
    msg.textContent = err.message || "Sign in failed.";
    msg.classList.add("show");
  }
});

async function postLogin(username, password) {
  const resp = await fetch("/api/v1/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password, device_name: "admin-web" }),
  });
  const data = await resp.json();
  if (!resp.ok) throw new Error(data.error || "Sign in failed.");
  return data;
}

el("logout").addEventListener("click", logout);

// ---- Dashboard ----
async function enterDashboard() {
  loginView.classList.add("hidden");
  dashView.classList.remove("hidden");
  await refreshAll();
}

async function refreshAll() {
  await Promise.all([loadLibraries(), loadUsers(), loadShares()]);
}

// Libraries
el("lib-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/libraries", {
      name: el("lib-name").value.trim(),
      root: el("lib-root").value.trim(),
      layout: el("lib-layout").value,
    });
    el("lib-form").reset();
    flash("Library added — scanning in the background.");
    await loadLibraries();
  } catch (err) { flash(err.message, "error"); }
});

async function loadLibraries() {
  const { libraries } = await api("GET", "/admin/libraries");
  librariesCache = libraries || [];
  const rows = el("lib-rows");
  rows.innerHTML = "";
  (libraries || []).forEach((l) => {
    const tr = document.createElement("tr");
    tr.append(
      td(l.id), td(l.name), td(l.root, "code"), layoutCell(l),
      actionTd(scanBtn(l.id), deleteLibBtn(l)),
    );
    rows.appendChild(tr);
  });
  fillSelect("a-lib", libraries, (l) => l.name);
}

// layoutCell renders an editable layout dropdown that PATCHes the library and
// triggers a rescan (changing the layout changes how books are discovered).
const LAYOUTS = ["books_in_folder", "chapters_in_folder", "flat"];
function layoutCell(l) {
  const d = document.createElement("td");
  const sel = document.createElement("select");
  LAYOUTS.forEach((opt) => {
    const o = document.createElement("option");
    o.value = opt;
    o.textContent = opt;
    if (opt === l.layout) o.selected = true;
    sel.appendChild(o);
  });
  sel.addEventListener("change", async () => {
    try {
      await api("PATCH", `/admin/libraries/${l.id}`, { layout: sel.value });
      flash(`Layout set to ${sel.value} — rescanning.`);
    } catch (err) {
      flash(err.message, "error");
      sel.value = l.layout;
    }
  });
  d.appendChild(sel);
  return d;
}

function deleteLibBtn(l) {
  return button("Delete", "danger small", async () => {
    if (!confirm(`Delete library "${l.name}"? Files on disk are kept; only the index is removed.`)) return;
    try {
      await api("DELETE", `/admin/libraries/${l.id}`);
      flash("Library deleted.");
      await loadLibraries();
    } catch (err) {
      flash(err.message, "error");
    }
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function scanBtn(id) {
  const b = button("Rescan", "secondary small", async () => {
    try {
      await api("POST", `/admin/libraries/${id}/scan`);
      flash("Rescan started.");
      pollScan(id, b);
    } catch (err) { flash(err.message, "error"); }
  });
  pollScan(id, b); // reflect a scan already running (e.g. after a page refresh)
  return b;
}

// pollScan updates the button label with live progress until the scan finishes.
async function pollScan(id, b) {
  for (;;) {
    let p;
    try { p = await api("GET", `/admin/libraries/${id}/scan`); }
    catch { break; }
    if (!p || !p.running) {
      if (b.disabled) flash("Rescan complete.");
      b.disabled = false;
      b.textContent = "Rescan";
      break;
    }
    b.disabled = true;
    b.textContent = p.total ? `Scanning ${p.done}/${p.total}…` : "Scanning…";
    await sleep(1000);
  }
}

// Users
el("user-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/users", {
      username: el("u-name").value.trim(),
      password: el("u-pass").value,
      role: el("u-role").value,
    });
    el("user-form").reset();
    flash("User created.");
    await loadUsers();
  } catch (err) { flash(err.message, "error"); }
});

async function loadUsers() {
  const { users } = await api("GET", "/admin/users");
  usersCache = users || [];
  const rows = el("user-rows");
  rows.innerHTML = "";
  (users || []).forEach((u) => {
    const tr = document.createElement("tr");
    const name = document.createElement("td");
    name.append(u.username + " ");
    if (u.role === "admin") name.append(pill("admin", "admin"));
    if (u.disabled) name.append(pill("disabled", "off"));
    tr.append(name, td(u.role), actionTd(authCodeBtn(u.id), disableBtn(u)));
    rows.appendChild(tr);
  });
  fillSelect("a-user", users, (u) => u.username);
}

function authCodeBtn(userId) {
  return button("Auth code", "secondary small", async () => {
    try {
      const { auth_code } = await api("POST", `/admin/users/${userId}/authcode`, { label: "admin-web" });
      flash("Auth code (copy now): " + auth_code);
    } catch (err) { flash(err.message, "error"); }
  });
}

function disableBtn(u) {
  return button(u.disabled ? "Enable" : "Disable", "danger small", async () => {
    try {
      await api("POST", `/admin/users/${u.id}/disable`, { disabled: !u.disabled });
      await loadUsers();
    } catch (err) { flash(err.message, "error"); }
  });
}

// Whole-library access (sugar over a "" rule share)
el("access-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/library-access", {
      user_id: Number(el("a-user").value),
      library_id: Number(el("a-lib").value),
    });
    flash("Whole-library access granted.");
    await loadShares();
  } catch (err) { flash(err.message, "error"); }
});

// ---- Shares ----
let usersCache = [];
let librariesCache = [];

el("share-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/shares", { name: el("s-name").value.trim() });
    el("share-form").reset();
    flash("Share created.");
    await loadShares();
  } catch (err) { flash(err.message, "error"); }
});

async function loadShares() {
  const { shares } = await api("GET", "/admin/shares");
  const list = el("share-list");
  list.innerHTML = "";
  (shares || []).forEach((s) => list.appendChild(shareCard(s)));
}

function libName(id) {
  const l = librariesCache.find((x) => x.id === id);
  return l ? l.name : `lib ${id}`;
}

function shareCard(s) {
  const card = document.createElement("div");
  card.className = "card";
  card.style.marginTop = "12px";

  const head = document.createElement("div");
  head.className = "topbar";
  head.style.marginBottom = "8px";
  const title = document.createElement("strong");
  title.textContent = s.name;
  head.append(title, deleteShareBtn(s));
  card.appendChild(head);

  // Path rules as chips.
  const chips = document.createElement("div");
  chips.className = "inline";
  (s.paths || []).forEach((r) => {
    const label = libName(r.library_id) + (r.path ? " › " + r.path : " (whole library)");
    const chip = document.createElement("span");
    chip.className = "pill";
    chip.textContent = label + "  ✕";
    chip.style.cursor = "pointer";
    chip.title = "Remove";
    chip.addEventListener("click", async () => {
      try {
        await api("DELETE", `/admin/shares/${s.id}/paths`, { library_id: r.library_id, path: r.path });
        await loadShares();
      } catch (err) { flash(err.message, "error"); }
    });
    chips.appendChild(chip);
  });
  if (!(s.paths || []).length) {
    const none = document.createElement("span");
    none.className = "muted";
    none.textContent = "No paths yet — add one.";
    chips.appendChild(none);
  }
  card.appendChild(chips);

  // Add path + grant-to-user controls.
  const controls = document.createElement("div");
  controls.className = "inline";
  controls.style.marginTop = "12px";
  controls.append(
    button("Browse & add path", "secondary small", () => openPicker(s)),
    grantToUserControl(s),
  );
  card.appendChild(controls);
  return card;
}

function deleteShareBtn(s) {
  return button("Delete share", "danger small", async () => {
    if (!confirm(`Delete share "${s.name}"? Users lose the access it granted.`)) return;
    try { await api("DELETE", `/admin/shares/${s.id}`); flash("Share deleted."); await loadShares(); }
    catch (err) { flash(err.message, "error"); }
  });
}

function grantToUserControl(s) {
  const wrap = document.createElement("span");
  wrap.className = "inline";
  const sel = document.createElement("select");
  sel.style.maxWidth = "180px";
  usersCache.forEach((u) => {
    const o = document.createElement("option");
    o.value = u.id;
    o.textContent = u.username;
    sel.appendChild(o);
  });
  wrap.append(sel, button("Grant to user", "small", async () => {
    if (!sel.value) return;
    try {
      await api("POST", "/admin/share-access", { user_id: Number(sel.value), share_id: s.id });
      flash("Share granted.");
    } catch (err) { flash(err.message, "error"); }
  }));
  return wrap;
}

// ---- Inline filesystem path picker ----
let pickerShare = null;
let pickerLib = null;
let pickerPath = "";

function openPicker(share) {
  pickerShare = share;
  el("picker-share").textContent = share.name;
  fillSelect("picker-lib", librariesCache, (l) => l.name);
  pickerLib = librariesCache.length ? librariesCache[0].id : null;
  el("picker-lib").value = pickerLib;
  el("picker").classList.remove("hidden");
  el("picker").scrollIntoView({ behavior: "smooth" });
  pickerNavigate("");
}

el("picker-close").addEventListener("click", () => el("picker").classList.add("hidden"));
el("picker-lib").addEventListener("change", () => {
  pickerLib = Number(el("picker-lib").value);
  pickerNavigate("");
});

async function addPickedPath(path) {
  try {
    await api("POST", `/admin/shares/${pickerShare.id}/paths`, { library_id: Number(pickerLib), path });
    flash(path ? `Added "${path}".` : "Added whole library.");
    el("picker").classList.add("hidden");
    await loadShares();
  } catch (err) { flash(err.message, "error"); }
}

async function pickerNavigate(path) {
  pickerPath = path;
  el("picker-crumb").textContent = "/" + path;
  const rows = el("picker-rows");
  rows.innerHTML = "";

  // Row: add the current location (whole library at root, or this folder).
  const here = document.createElement("tr");
  const hereLabel = document.createElement("td");
  hereLabel.innerHTML = path ? `<em>this folder</em>` : `<em>whole library</em>`;
  here.append(hereLabel, actionTd(button(path ? "Share this folder" : "Share whole library", "small", () => addPickedPath(path))));
  rows.appendChild(here);

  let listing;
  try {
    listing = await api("GET", `/libraries/${pickerLib}/fs?path=${encodeURIComponent(path)}&limit=500`);
  } catch (err) { flash(err.message, "error"); return; }

  if (path) {
    const up = document.createElement("tr");
    const upName = document.createElement("td");
    const parent = path.split("/").slice(0, -1).join("/");
    upName.innerHTML = `<a href="#">⬆ ..</a>`;
    upName.querySelector("a").addEventListener("click", (e) => { e.preventDefault(); pickerNavigate(parent); });
    up.append(upName, document.createElement("td"));
    rows.appendChild(up);
  }

  (listing.entries || []).forEach((entry) => {
    const tr = document.createElement("tr");
    const name = document.createElement("td");
    if (entry.is_dir) {
      const a = document.createElement("a");
      a.href = "#";
      a.textContent = "📁 " + entry.name;
      a.addEventListener("click", (e) => { e.preventDefault(); pickerNavigate(entry.path); });
      name.appendChild(a);
    } else {
      name.textContent = (entry.is_audio ? "🎧 " : "📄 ") + entry.name;
    }
    const action = entry.is_dir || entry.is_audio
      ? actionTd(button("Share this", "small", () => addPickedPath(entry.path)))
      : document.createElement("td");
    tr.append(name, action);
    rows.appendChild(tr);
  });
}

// ---- DOM helpers ----
function td(text, cls) {
  const d = document.createElement("td");
  if (cls) { const s = document.createElement("span"); s.className = cls; s.textContent = text; d.appendChild(s); }
  else d.textContent = text;
  return d;
}
function actionTd(...nodes) {
  const d = document.createElement("td");
  const wrap = document.createElement("div");
  wrap.className = "inline";
  nodes.forEach((n) => wrap.appendChild(n));
  d.appendChild(wrap);
  return d;
}
function button(label, cls, onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = cls;
  b.textContent = label;
  b.addEventListener("click", onClick);
  return b;
}
function pill(text, cls) {
  const s = document.createElement("span");
  s.className = "pill " + (cls || "");
  s.textContent = text;
  return s;
}
function fillSelect(id, items, labelFn) {
  const sel = el(id);
  const prev = sel.value;
  sel.innerHTML = "";
  (items || []).forEach((it) => {
    const o = document.createElement("option");
    o.value = it.id;
    o.textContent = labelFn(it);
    sel.appendChild(o);
  });
  if (prev) sel.value = prev;
}

// ---- Boot ----
(async function boot() {
  if (!token) return; // show login
  try {
    const me = await api("GET", "/me");
    if (me.role !== "admin") { logout(); return; }
    el("who").textContent = me.username;
    await enterDashboard();
  } catch (_) { /* api() already handled 401 by logging out */ }
})();
