// Admin console. A static client over the JSON API; the server enforces the
// admin role on every /admin/* call, so this file holds no privileged logic.
// All behaviour is wired with addEventListener (no inline handlers) and all
// styling comes from style.css — the page runs under a strict same-origin CSP.
const TOKEN_KEY = "audiosilo_token";
let token = localStorage.getItem(TOKEN_KEY);

let usersCache = [];
let librariesCache = [];
let sharesCache = [];

const el = (id) => document.getElementById(id);
const loginView = el("login-view");
const app = el("app");

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

// ---- Toast ----
let toastTimer = null;
function toast(text, kind) {
  const t = el("toast");
  t.textContent = text;
  t.className = "toast show " + (kind || "ok");
  clearTimeout(toastTimer);
  if (kind !== "error") toastTimer = setTimeout(() => t.classList.remove("show"), 4000);
}

function logout() {
  token = null;
  localStorage.removeItem(TOKEN_KEY);
  app.classList.add("hidden");
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

// ---- Section nav ----
const SECTIONS = ["overview", "libraries", "users", "shares"];
document.querySelectorAll("#nav button").forEach((b) =>
  b.addEventListener("click", () => showSection(b.dataset.section)));

function showSection(name) {
  if (!SECTIONS.includes(name)) name = "overview";
  document.querySelectorAll("#nav button").forEach((b) =>
    b.classList.toggle("active", b.dataset.section === name));
  SECTIONS.forEach((s) => el("sec-" + s).classList.toggle("active", s === name));
  if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
  if (name === "overview") loadStats();
}

// ---- Dashboard ----
async function enterDashboard() {
  loginView.classList.add("hidden");
  app.classList.remove("hidden");
  await Promise.all([loadLibraries(), loadUsers(), loadShares()]);
  showSection((location.hash || "#overview").slice(1));
}

// ================= Overview / Stats =================
async function loadStats() {
  let s;
  try { s = await api("GET", "/admin/stats"); }
  catch (err) { toast(err.message, "error"); return; }

  const active = (s.listening || []).filter((r) => !r.finished).length;
  const grid = el("stat-grid");
  grid.innerHTML = "";
  grid.append(
    statCard(s.total_books, "Books"),
    statCard(s.total_libraries, "Libraries"),
    statCard(s.total_users, "Users"),
    statCard(active, "Listening now"),
  );

  const counts = el("lib-counts");
  counts.innerHTML = "";
  if (!(s.libraries || []).length) counts.append(emptyNote("No libraries yet."));
  (s.libraries || []).forEach((l) => {
    const row = div("kv");
    row.append(span(l.name), span(l.book_count + " books"));
    counts.append(row);
  });

  const list = el("listening");
  list.innerHTML = "";
  if (!(s.listening || []).length) { list.append(emptyNote("Nobody is listening yet.")); return; }
  (s.listening || []).forEach((r) => list.append(listenRow(r)));
}

function statCard(n, k) {
  const c = div("stat");
  c.append(div("n", String(n)), div("k", k));
  return c;
}

function listenRow(r) {
  const pct = r.duration > 0 ? Math.min(100, Math.round((r.position / r.duration) * 100)) : (r.finished ? 100 : 0);
  const row = div("listen-row" + (r.finished ? " done" : ""));
  row.append(div("who", r.username));
  const meta = div("meta");
  const title = r.title || baseName(r.path) || "(unknown)";
  const sub = (r.author ? r.author + " · " : "") + fmtRelative(r.updated_at);
  meta.append(div("t", title), div("sub", sub));
  const bar = div("bar");
  const fill = document.createElement("span");
  fill.style.width = pct + "%";
  bar.append(fill);
  meta.append(bar);
  row.append(meta, div("pct", r.finished ? "done" : pct + "%"));
  return row;
}

// ================= Libraries =================
el("add-lib-btn").addEventListener("click", () => openModal("modal-library"));

el("lib-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/libraries", {
      name: el("lib-name").value.trim(),
      root: el("lib-root").value.trim(),
      layout: el("lib-layout").value,
    });
    el("lib-form").reset();
    closeModals();
    toast("Library added — scanning in the background.");
    await loadLibraries();
  } catch (err) { toast(err.message, "error"); }
});

async function loadLibraries() {
  const { libraries } = await api("GET", "/admin/libraries");
  librariesCache = libraries || [];
  const rows = el("lib-rows");
  rows.innerHTML = "";
  librariesCache.forEach((l) => {
    const tr = document.createElement("tr");
    tr.append(td(l.name), td(l.root, "code"), layoutCell(l), actionTd(scanBtn(l.id), deleteLibBtn(l)));
    rows.appendChild(tr);
  });
  if (!librariesCache.length) {
    const tr = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 4;
    cell.append(emptyNote("No libraries yet — add one to start scanning."));
    tr.append(cell);
    rows.appendChild(tr);
  }
}

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
      toast(`Layout set to ${sel.value} — rescanning.`);
    } catch (err) { toast(err.message, "error"); sel.value = l.layout; }
  });
  d.appendChild(sel);
  return d;
}

function deleteLibBtn(l) {
  return button("Delete", "danger small", async () => {
    if (!confirm(`Delete library "${l.name}"? Files on disk are kept; only the index is removed.`)) return;
    try { await api("DELETE", `/admin/libraries/${l.id}`); toast("Library deleted."); await loadLibraries(); }
    catch (err) { toast(err.message, "error"); }
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function scanBtn(id) {
  const b = button("Rescan", "secondary small", async () => {
    try { await api("POST", `/admin/libraries/${id}/scan`); toast("Rescan started."); pollScan(id, b); }
    catch (err) { toast(err.message, "error"); }
  });
  pollScan(id, b); // reflect a scan already running (e.g. after a page refresh)
  return b;
}

async function pollScan(id, b) {
  for (;;) {
    let p;
    try { p = await api("GET", `/admin/libraries/${id}/scan`); }
    catch { break; }
    if (!p || !p.running) {
      if (b.disabled) toast("Rescan complete.");
      b.disabled = false;
      b.textContent = "Rescan";
      break;
    }
    b.disabled = true;
    b.textContent = p.total ? `Scanning ${p.done}/${p.total}…` : "Scanning…";
    await sleep(1000);
  }
}

// ================= Users =================
el("add-user-btn").addEventListener("click", () => {
  el("user-form").reset();
  updatePassHint();
  updateAccessFields();
  fillMultiSelect("u-access-libs", librariesCache, (l) => l.name);
  fillMultiSelect("u-access-shares", sharesCache, (s) => s.name);
  openModal("modal-user");
});
el("u-role").addEventListener("change", updatePassHint);
el("u-access").addEventListener("change", updateAccessFields);

function updatePassHint() {
  const admin = el("u-role").value === "admin";
  el("u-pass").required = admin;
  el("u-pass-hint").textContent = admin
    ? "Required — admins sign in to this console."
    : "Optional — leave blank for a player-only account that pairs via an invite code.";
}

// updateAccessFields reveals the libraries / shares multiselect that matches the
// chosen access kind (none reveals neither).
function updateAccessFields() {
  const v = el("u-access").value;
  el("u-access-libs-field").classList.toggle("hidden", v !== "library");
  el("u-access-shares-field").classList.toggle("hidden", v !== "shares");
}

el("user-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const u = await api("POST", "/admin/users", {
      username: el("u-name").value.trim(),
      password: el("u-pass").value,
      role: el("u-role").value,
    });
    // Apply the initial access selection (best-effort, after the account exists).
    const kind = el("u-access").value;
    if (kind === "library") {
      for (const id of selectedValues("u-access-libs")) {
        await api("POST", "/admin/library-access", { user_id: u.id, library_id: id });
      }
    } else if (kind === "shares") {
      for (const id of selectedValues("u-access-shares")) {
        await api("POST", "/admin/share-access", { user_id: u.id, share_id: id });
      }
    }
    el("user-form").reset();
    closeModals();
    toast("User created.");
    await Promise.all([loadUsers(), loadShares()]);
  } catch (err) { toast(err.message, "error"); }
});

async function loadUsers() {
  const { users } = await api("GET", "/admin/users");
  usersCache = users || [];
  const rows = el("user-rows");
  rows.innerHTML = "";
  usersCache.forEach((u) => {
    const tr = document.createElement("tr");
    tr.className = "clickable";
    const name = document.createElement("td");
    const strong = document.createElement("strong");
    strong.textContent = u.username;
    name.append(strong, " ");
    if (u.disabled) name.append(pill("disabled", "off"));
    if (!u.has_password && u.role !== "admin") name.append(pill("invite-only", "muted"));

    const role = document.createElement("td");
    role.append(u.role === "admin" ? pill("admin", "admin") : span(u.role));

    const chev = document.createElement("td");
    chev.append(span("›", "right"));

    tr.append(name, role, td(fmtRelative(u.last_seen_at)), chev);
    tr.addEventListener("click", () => openUserDrawer(u.id));
    rows.appendChild(tr);
  });
}

// ================= User detail drawer =================
function openUserDrawer(userId) {
  el("drawer-scrim").classList.add("show");
  const drawer = el("user-drawer");
  drawer.classList.add("show");
  drawer.setAttribute("aria-hidden", "false");
  renderUserDrawer(userId);
}
function closeDrawer() {
  el("drawer-scrim").classList.remove("show");
  const drawer = el("user-drawer");
  drawer.classList.remove("show");
  drawer.setAttribute("aria-hidden", "true");
}
el("drawer-close").addEventListener("click", closeDrawer);
el("drawer-scrim").addEventListener("click", closeDrawer);

async function renderUserDrawer(userId) {
  const body = el("drawer-body");
  body.innerHTML = "";
  el("drawer-title").textContent = "";
  let d;
  try { d = await api("GET", `/admin/users/${userId}`); }
  catch (err) { toast(err.message, "error"); return; }
  const u = d.user;
  el("drawer-title").textContent = u.username;

  // --- Account ---
  const account = block("Account");
  account.append(
    kvNode("Role", roleControl(u)),
    kvNode("Status", statusControl(u)),
    kvNode("Password", passwordControl(u)),
    kvNode("Last active", textNode(fmtRelative(u.last_seen_at))),
  );
  body.append(account);

  // --- Access ---
  const access = block("Access");
  if (u.role === "admin") {
    access.append(textNode("Administrator — full access to all libraries."));
  } else {
    const shares = d.shares || [];
    access.append(subLabel("Granted access"));
    if (shares.length) shares.forEach((s) => access.append(grantRow(s, u.id)));
    else access.append(emptyNote("No access granted yet."));
    access.append(spacerNode(), subLabel("Grant access"), accessAdder(u.id));
  }
  body.append(access);

  // --- Invite codes ---
  const codesBlock = block("Invite codes");
  const codes = d.auth_codes || [];
  if (!codes.length) codesBlock.append(emptyNote("No invite codes issued."));
  codes.forEach((c) => codesBlock.append(codeCard(c, userId)));
  const actions = div("inline");
  actions.append(button("Create invite", "secondary small", () => openInvite(userId)));
  codesBlock.append(spacerNode(), actions);
  body.append(codesBlock);
}

function roleControl(u) {
  const sel = document.createElement("select");
  ["user", "admin"].forEach((r) => {
    const o = document.createElement("option");
    o.value = r; o.textContent = r;
    if (u.role === r) o.selected = true;
    sel.appendChild(o);
  });
  sel.addEventListener("change", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { role: sel.value });
      toast(`Role changed to ${sel.value}.`);
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); sel.value = u.role; }
  });
  return sel;
}

function statusControl(u) {
  const wrap = div("inline");
  wrap.append(pill(u.disabled ? "disabled" : "enabled", u.disabled ? "off" : "ok"));
  wrap.append(button(u.disabled ? "Enable" : "Disable", "small", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { disabled: !u.disabled });
      toast(u.disabled ? "User enabled." : "User disabled.");
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); }
  }));
  return wrap;
}

function passwordControl(u) {
  const wrap = div("inline");
  wrap.append(pill(u.has_password ? "set" : "none", u.has_password ? "ok" : "muted"));
  const inp = document.createElement("input");
  inp.type = "password";
  inp.placeholder = "New password";
  inp.classList.add("hidden");
  const save = button("Save", "small", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { password: inp.value });
      toast("Password updated.");
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); }
  });
  save.classList.add("hidden");
  const setBtn = button(u.has_password ? "Change" : "Set password", "small", () => {
    inp.classList.toggle("hidden");
    save.classList.toggle("hidden");
    if (!inp.classList.contains("hidden")) inp.focus();
  });
  wrap.append(setBtn, inp, save);
  return wrap;
}

function codeCard(c, userId) {
  const card = div("codecard");
  const top = div("top");
  const label = document.createElement("strong");
  label.textContent = c.label || "invite";
  const exp = expiryLabel(c.expires_at);
  const tail = div("inline");
  tail.append(pill(exp.text, exp.cls));
  tail.append(button("Revoke", "danger small", async () => {
    try { await api("DELETE", `/admin/authcodes/${c.id}`); toast("Code revoked."); renderUserDrawer(userId); }
    catch (err) { toast(err.message, "error"); }
  }));
  top.append(label, tail);
  card.append(top);
  const sub = div("sub");
  sub.append(span("Issued " + fmtRelative(c.created_at)), span(usesLabel(c)));
  card.append(sub);
  return card;
}

// grantRow renders one access grant (a named share or a whole-library sugar
// share) with its path chips and a revoke button.
function grantRow(s, userId) {
  const sc = div("codecard");
  const top = div("top");
  const nm = document.createElement("strong");
  nm.textContent = s.name;
  top.append(nm, button("Revoke", "danger small", async () => {
    try { await api("DELETE", "/admin/share-access", { user_id: userId, share_id: s.id }); toast("Access revoked."); await loadShares(); renderUserDrawer(userId); }
    catch (err) { toast(err.message, "error"); }
  }));
  sc.append(top);
  const chips = div("inline");
  (s.paths || []).forEach((r) => chips.append(chip(libName(r.library_id) + (r.path ? " › " + r.path : " (whole library)"))));
  if ((s.paths || []).length) sc.append(chips);
  return sc;
}

// accessAdder builds the "grant access" control: a kind dropdown (whole library
// or a named share) whose target select repopulates to match, plus a Grant button.
function accessAdder(userId) {
  const wrap = div("inline");
  const kind = document.createElement("select");
  [["library", "Whole library"], ["share", "Share"]].forEach(([v, l]) => {
    const o = document.createElement("option");
    o.value = v; o.textContent = l;
    kind.append(o);
  });
  const target = document.createElement("select");
  function fillTarget() {
    target.innerHTML = "";
    (kind.value === "library" ? librariesCache : sharesCache).forEach((it) => {
      const o = document.createElement("option");
      o.value = it.id; o.textContent = it.name;
      target.append(o);
    });
  }
  kind.addEventListener("change", fillTarget);
  fillTarget();
  const grant = button("Grant", "small", async () => {
    if (!target.value) return;
    try {
      if (kind.value === "library") await api("POST", "/admin/library-access", { user_id: userId, library_id: Number(target.value) });
      else await api("POST", "/admin/share-access", { user_id: userId, share_id: Number(target.value) });
      toast("Access granted.");
      await loadShares();
      renderUserDrawer(userId);
    } catch (err) { toast(err.message, "error"); }
  });
  wrap.append(kind, target, grant);
  return wrap;
}

// ---- Invite creation ----
// One dialog mints a code with chosen uses/expiry and shows both the invite link
// and the raw code, each copyable. The code is returned once, so this is the only
// chance to copy it.
let inviteUserId = null;

function openInvite(userId) {
  inviteUserId = userId;
  el("invite-form").reset(); // restores the selected defaults (5 uses / 1 day)
  el("inv-uses-custom").classList.add("hidden");
  el("inv-expiry-custom").classList.add("hidden");
  el("invite-form").classList.remove("hidden");
  el("invite-result").classList.add("hidden");
  openModal("modal-invite");
}

// Reveal a "Custom…" number input only when its select chooses custom.
function wireCustom(selectId, inputId) {
  const sel = el(selectId), inp = el(inputId);
  sel.addEventListener("change", () => {
    const custom = sel.value === "custom";
    inp.classList.toggle("hidden", !custom);
    if (custom) inp.focus();
  });
}
wireCustom("inv-uses", "inv-uses-custom");
wireCustom("inv-expiry", "inv-expiry-custom");

// Resolve a preset select (+ its custom input) to an integer (0 = unlimited/never
// for the fixed options), or null when a custom value is required but invalid.
function presetValue(selectId, inputId) {
  const sel = el(selectId);
  if (sel.value !== "custom") return Number(sel.value);
  const n = parseInt(el(inputId).value, 10);
  return Number.isFinite(n) && n >= 1 ? n : null;
}

el("invite-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const maxUses = presetValue("inv-uses", "inv-uses-custom");
  const ttlDays = presetValue("inv-expiry", "inv-expiry-custom");
  if (maxUses === null || ttlDays === null) { toast("Enter a custom value of 1 or more.", "error"); return; }
  try {
    const data = await api("POST", `/admin/users/${inviteUserId}/authcode`,
      { label: "invite", max_uses: maxUses, ttl_days: ttlDays });
    el("inv-link").textContent = data.invite_url;
    el("inv-code").textContent = data.auth_code;
    el("invite-form").classList.add("hidden");
    el("invite-result").classList.remove("hidden");
  } catch (err) { toast(err.message, "error"); }
});

async function copyField(text, label) {
  if (await copyToClipboard(text)) toast(label + " copied.");
  else toast(label + " (copy now): " + text);
}
el("inv-copy-link").addEventListener("click", () => copyField(el("inv-link").textContent, "Invite link"));
el("inv-copy-code").addEventListener("click", () => copyField(el("inv-code").textContent, "Auth code"));
el("inv-done").addEventListener("click", () => {
  closeModals();
  if (inviteUserId != null) renderUserDrawer(inviteUserId);
});

async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch { /* fall through to legacy path */ }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.classList.add("offscreen");
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch { return false; }
}

// ================= Shares =================
el("add-share-btn").addEventListener("click", () => openModal("modal-share"));

el("share-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/admin/shares", { name: el("s-name").value.trim() });
    el("share-form").reset();
    closeModals();
    toast("Share created.");
    await loadShares();
  } catch (err) { toast(err.message, "error"); }
});

async function loadShares() {
  const { shares } = await api("GET", "/admin/shares");
  sharesCache = shares || [];
  const list = el("share-list");
  list.innerHTML = "";
  if (!sharesCache.length) { list.append(card(emptyNote("No shares yet — create one, then grant it from the Users page."))); return; }
  sharesCache.forEach((s) => list.appendChild(shareCard(s)));
}

function libName(id) {
  const l = librariesCache.find((x) => x.id === id);
  return l ? l.name : `lib ${id}`;
}

function shareCard(s) {
  const c = card();
  const head = div("page-head");
  const title = document.createElement("h2");
  title.textContent = s.name;
  head.append(title, deleteShareBtn(s));
  c.appendChild(head);

  const chips = div("inline");
  (s.paths || []).forEach((r) => {
    const label = libName(r.library_id) + (r.path ? " › " + r.path : " (whole library)");
    const ch = document.createElement("span");
    ch.className = "chip";
    ch.append(document.createTextNode(label));
    const x = document.createElement("span");
    x.className = "x";
    x.textContent = "✕";
    x.title = "Remove path";
    x.addEventListener("click", async () => {
      try { await api("DELETE", `/admin/shares/${s.id}/paths`, { library_id: r.library_id, path: r.path }); await loadShares(); }
      catch (err) { toast(err.message, "error"); }
    });
    ch.append(x);
    chips.appendChild(ch);
  });
  if (!(s.paths || []).length) chips.append(emptyNote("No paths yet — add one."));
  c.appendChild(chips);

  const controls = div("inline");
  controls.append(button("Browse & add path", "secondary small", () => openPicker(s)));
  c.appendChild(spacerNode());
  c.appendChild(controls);
  return c;
}

function deleteShareBtn(s) {
  return button("Delete share", "danger small", async () => {
    if (!confirm(`Delete share "${s.name}"? Users lose the access it granted.`)) return;
    try { await api("DELETE", `/admin/shares/${s.id}`); toast("Share deleted."); await loadShares(); }
    catch (err) { toast(err.message, "error"); }
  });
}

// ---- Filesystem path picker (modal) ----
let pickerShare = null;
let pickerLib = null;

function openPicker(share) {
  pickerShare = share;
  el("picker-share").textContent = share.name;
  fillSelect("picker-lib", librariesCache, (l) => l.name);
  pickerLib = librariesCache.length ? librariesCache[0].id : null;
  if (pickerLib != null) el("picker-lib").value = pickerLib;
  openModal("modal-picker");
  pickerNavigate("");
}

el("picker-lib").addEventListener("change", () => {
  pickerLib = Number(el("picker-lib").value);
  pickerNavigate("");
});

async function addPickedPath(path) {
  try {
    await api("POST", `/admin/shares/${pickerShare.id}/paths`, { library_id: Number(pickerLib), path });
    toast(path ? `Added "${path}".` : "Added whole library.");
    closeModals();
    await loadShares();
  } catch (err) { toast(err.message, "error"); }
}

async function pickerNavigate(path) {
  el("picker-crumb").textContent = "/" + path;
  const rows = el("picker-rows");
  rows.innerHTML = "";

  const here = document.createElement("tr");
  const hereLabel = document.createElement("td");
  const em = document.createElement("em");
  em.textContent = path ? "this folder" : "whole library";
  hereLabel.append(em);
  here.append(hereLabel, actionTd(button(path ? "Share this folder" : "Share whole library", "small", () => addPickedPath(path))));
  rows.appendChild(here);

  let listing;
  try { listing = await api("GET", `/libraries/${pickerLib}/fs?path=${encodeURIComponent(path)}&limit=500`); }
  catch (err) { toast(err.message, "error"); return; }

  if (path) {
    const up = document.createElement("tr");
    const upName = document.createElement("td");
    const parent = path.split("/").slice(0, -1).join("/");
    const a = document.createElement("a");
    a.href = "#";
    a.textContent = "⬆ ..";
    a.addEventListener("click", (e) => { e.preventDefault(); pickerNavigate(parent); });
    upName.append(a);
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

// ================= Modal plumbing =================
function openModal(id) { el(id).classList.add("show"); }
function closeModals() { document.querySelectorAll(".modal-scrim.show").forEach((m) => m.classList.remove("show")); }

document.querySelectorAll(".modal-scrim").forEach((scrim) => {
  scrim.addEventListener("click", (e) => { if (e.target === scrim) closeModals(); });
});
document.querySelectorAll("[data-close]").forEach((b) => b.addEventListener("click", closeModals));
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") { closeModals(); closeDrawer(); }
});

// ================= DOM helpers =================
function div(cls, text) { const d = document.createElement("div"); if (cls) d.className = cls; if (text != null) d.textContent = text; return d; }
function span(text, cls) { const s = document.createElement("span"); if (cls) s.className = cls; s.textContent = text; return s; }
function textNode(text) { return document.createTextNode(text); }
function spacerNode() { return div("spacer"); }
function emptyNote(text) { return div("empty", text); }
function subLabel(text) { const l = document.createElement("label"); l.textContent = text; return l; }
function card(child) { const c = div("card"); if (child) c.append(child); return c; }
function block(heading) { const b = div("block"); const h = document.createElement("h3"); h.textContent = heading; b.append(h); return b; }
function kvNode(k, valNode) {
  const row = div("kv");
  row.append(span(k, "k"));
  const v = document.createElement("span");
  v.append(valNode);
  row.append(v);
  return row;
}
function chip(text) { const c = document.createElement("span"); c.className = "chip"; c.textContent = text; return c; }

function td(text, cls) {
  const d = document.createElement("td");
  if (cls) { const s = document.createElement("span"); s.className = cls; s.textContent = text; d.appendChild(s); }
  else d.textContent = text;
  return d;
}
function actionTd(...nodes) {
  const d = document.createElement("td");
  const wrap = div("inline");
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
function pill(text, cls) { const s = document.createElement("span"); s.className = "pill " + (cls || ""); s.textContent = text; return s; }
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
function fillMultiSelect(id, items, labelFn) {
  const sel = el(id);
  sel.innerHTML = "";
  (items || []).forEach((it) => {
    const o = document.createElement("option");
    o.value = it.id;
    o.textContent = labelFn(it);
    sel.appendChild(o);
  });
}
function selectedValues(id) {
  return Array.from(el(id).selectedOptions).map((o) => Number(o.value));
}

// ---- Formatting ----
function baseName(p) { if (!p) return ""; const parts = p.split("/"); return parts[parts.length - 1]; }

function fmtRelative(ts) {
  if (!ts) return "never";
  const then = new Date(ts).getTime();
  if (isNaN(then)) return "—";
  const s = Math.floor((Date.now() - then) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  if (s < 86400 * 30) return Math.floor(s / 86400) + "d ago";
  return new Date(ts).toLocaleDateString();
}

function expiryLabel(expiresAt) {
  if (!expiresAt) return { text: "no expiry", cls: "muted" };
  const ms = new Date(expiresAt).getTime() - Date.now();
  if (ms <= 0) return { text: "expired", cls: "off" };
  return { text: shortDur(ms) + " left", cls: "ok" };
}
function shortDur(ms) {
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${s}s`;
}
function usesLabel(c) {
  if (c.max_uses === 0) return c.uses > 0 ? `used ${c.uses}× (unlimited)` : "unused (unlimited)";
  const usedUp = c.uses >= c.max_uses;
  return `${c.uses}/${c.max_uses} used${usedUp ? " — used up" : ""}`;
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
