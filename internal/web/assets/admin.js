// Admin console. A static client over the JSON API; the server enforces the
// admin role on every /admin/* call, so this file holds no privileged logic.
// All behaviour is wired with addEventListener (no inline handlers) and all
// styling comes from style.css - the page runs under a strict same-origin CSP.
const TOKEN_KEY = "audiosilo_token";
let token = localStorage.getItem(TOKEN_KEY);

let usersCache = [];
let librariesCache = [];
let sharesCache = [];
let myUserId = null; // the signed-in admin's own id (to guard self-delete)

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
    throw new Error(asI18n.t("admin.toast.sessionExpired"));
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
      msg.textContent = asI18n.t("admin.login.notAdmin");
      msg.classList.add("show");
      return;
    }
    token = data.token;
    localStorage.setItem(TOKEN_KEY, token);
    myUserId = data.user.id;
    el("who").textContent = data.user.username;
    enterDashboard();
  } catch (err) {
    msg.textContent = err.message || asI18n.t("admin.login.failed");
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
  if (!resp.ok) throw new Error(data.error || asI18n.t("admin.login.failed"));
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
    statCard(s.total_books, asI18n.t("admin.stat.books")),
    statCard(s.total_libraries, asI18n.t("admin.stat.libraries")),
    statCard(s.total_users, asI18n.t("admin.stat.users")),
    statCard(active, asI18n.t("admin.stat.listeningNow")),
  );

  const counts = el("lib-counts");
  counts.innerHTML = "";
  if (!(s.libraries || []).length) counts.append(emptyNote(asI18n.t("admin.overview.noLibraries")));
  (s.libraries || []).forEach((l) => {
    const row = div("kv");
    row.append(span(l.name), span(asI18n.t("admin.overview.bookCount", { count: l.book_count })));
    counts.append(row);
  });

  const list = el("listening");
  list.innerHTML = "";
  if (!(s.listening || []).length) { list.append(emptyNote(asI18n.t("admin.overview.nobodyListening"))); return; }
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
  const title = r.title || baseName(r.path) || asI18n.t("admin.overview.unknownTitle");
  const sub = (r.author ? r.author + " · " : "") + fmtRelative(r.updated_at);
  meta.append(div("t", title), div("sub", sub));
  const bar = div("bar");
  const fill = document.createElement("span");
  fill.style.width = pct + "%";
  bar.append(fill);
  meta.append(bar);
  row.append(meta, div("pct", r.finished ? asI18n.t("admin.overview.done") : pct + "%"));
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
    });
    el("lib-form").reset();
    closeModals();
    toast(asI18n.t("admin.toast.libraryAdded"));
    await loadLibraries();
  } catch (err) { toast(err.message, "error"); }
});

async function loadLibraries() {
  const { libraries } = await api("GET", "/admin/libraries");
  librariesCache = libraries || [];
  const rows = el("lib-rows");
  rows.innerHTML = "";
  librariesCache.forEach((l, i) => {
    const tr = document.createElement("tr");
    tr.append(
      td(l.name),
      td(l.root, "code"),
      actionTd(...reorderBtns(i, librariesCache.length), detectBtn(l), scanBtn(l.id), deleteLibBtn(l)),
    );
    rows.appendChild(tr);
  });
  if (!librariesCache.length) {
    const tr = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 3;
    cell.append(emptyNote(asI18n.t("admin.libraries.empty")));
    tr.append(cell);
    rows.appendChild(tr);
  }
}

// Library order is the tiebreaker when the same book exists in more than one
// library: with all else equal, the copy in the higher (earlier) library wins
// de-duplication in search and "recently added". These buttons move a library
// up/down and persist the new order.
function reorderBtns(idx, count) {
  const up = button("↑", "secondary small", () => moveLib(idx, -1));
  const down = button("↓", "secondary small", () => moveLib(idx, 1));
  up.disabled = idx === 0;
  down.disabled = idx === count - 1;
  up.title = asI18n.t("admin.libraries.moveUp");
  down.title = asI18n.t("admin.libraries.moveDown");
  return [up, down];
}

async function moveLib(idx, delta) {
  const j = idx + delta;
  if (j < 0 || j >= librariesCache.length) return;
  const ids = librariesCache.map((x) => x.id);
  [ids[idx], ids[j]] = [ids[j], ids[idx]];
  try {
    await api("PUT", "/admin/libraries/order", { ids });
    await loadLibraries();
  } catch (err) {
    toast(err.message, "error");
  }
}

function detectBtn(l) {
  return button(asI18n.t("admin.libraries.detection"), "secondary small", () => openDetect(l));
}

function deleteLibBtn(l) {
  return button(asI18n.t("admin.common.delete"), "danger small", async () => {
    if (!confirm(asI18n.t("admin.confirm.deleteLibrary", { name: l.name }))) return;
    try { await api("DELETE", `/admin/libraries/${l.id}`); toast(asI18n.t("admin.toast.libraryDeleted")); await loadLibraries(); }
    catch (err) { toast(err.message, "error"); }
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function scanBtn(id) {
  const b = button(asI18n.t("admin.scan.rescan"), "secondary small", async () => {
    try { await api("POST", `/admin/libraries/${id}/scan`); toast(asI18n.t("admin.toast.rescanStarted")); pollScan(id, b); }
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
      if (b.disabled) toast(asI18n.t("admin.toast.rescanComplete"));
      b.disabled = false;
      b.textContent = asI18n.t("admin.scan.rescan");
      break;
    }
    b.disabled = true;
    b.textContent = p.total ? asI18n.t("admin.scan.scanningProgress", { done: p.done, total: p.total }) : asI18n.t("admin.scan.scanning");
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
    ? asI18n.t("admin.userModal.passHintRequired")
    : asI18n.t("admin.userModal.passHintOptional");
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
    toast(asI18n.t("admin.toast.userCreated"));
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
    if (u.disabled) name.append(pill(asI18n.t("admin.users.pillDisabled"), "off"));
    if (!u.has_password && u.role !== "admin") name.append(pill(asI18n.t("admin.users.pillInviteOnly"), "muted"));

    const role = document.createElement("td");
    role.append(u.role === "admin" ? pill(asI18n.t("admin.role.admin"), "admin") : span(roleLabel(u.role)));

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
  const account = block(asI18n.t("admin.drawer.account"));
  account.append(
    kvNode(asI18n.t("admin.drawer.role"), roleControl(u)),
    kvNode(asI18n.t("admin.drawer.status"), statusControl(u)),
    kvNode(asI18n.t("admin.drawer.password"), passwordControl(u)),
    kvNode(asI18n.t("admin.drawer.recoveryCode"), recoveryControl(u)),
    kvNode(asI18n.t("admin.drawer.lastActive"), textNode(fmtRelative(u.last_seen_at))),
  );
  body.append(account);

  // --- Access ---
  const access = block(asI18n.t("admin.drawer.access"));
  if (u.role === "admin") {
    access.append(textNode(asI18n.t("admin.drawer.adminAccess")));
  } else {
    const shares = d.shares || [];
    access.append(subLabel(asI18n.t("admin.drawer.grantedAccess")));
    if (shares.length) shares.forEach((s) => access.append(grantRow(s, u.id)));
    else access.append(emptyNote(asI18n.t("admin.drawer.noAccess")));
    access.append(spacerNode(), subLabel(asI18n.t("admin.drawer.grantAccess")), accessAdder(u.id));
  }
  body.append(access);

  // --- Invites ---
  // One active (pending) invite per user; accepted/expired/used-up ones collapse
  // into History so the list can't pile up. Recovery is separate (Account block).
  const codesBlock = block(asI18n.t("admin.drawer.invites"));
  const codes = d.auth_codes || [];
  const pending = codes.filter(codePending);
  const history = codes.filter((c) => !codePending(c));
  if (pending.length) pending.forEach((c) => codesBlock.append(codeCard(c, userId)));
  else codesBlock.append(emptyNote(asI18n.t("admin.drawer.noPendingInvite")));
  const actions = div("inline");
  actions.append(button(pending.length ? asI18n.t("admin.drawer.newInvite") : asI18n.t("admin.invite.title"), "secondary small", () => openInvite(userId)));
  codesBlock.append(spacerNode(), actions);
  if (history.length) codesBlock.append(historyDisclosure(history, userId));
  body.append(codesBlock);

  // --- Danger zone: delete account ---
  const danger = block(asI18n.t("admin.drawer.dangerZone"));
  if (u.id === myUserId) {
    danger.append(emptyNote(asI18n.t("admin.drawer.deleteSelfHint")));
  } else {
    danger.append(subLabel(asI18n.t("admin.drawer.deleteUserHint")));
    danger.append(
      button(asI18n.t("admin.drawer.deleteUser"), "danger small", async () => {
        if (!confirm(asI18n.t("admin.confirm.deleteUser", { name: u.username }))) return;
        try {
          await api("DELETE", `/admin/users/${u.id}`);
          toast(asI18n.t("admin.toast.userDeleted"));
          closeDrawer();
          await loadUsers();
        } catch (err) { toast(err.message, "error"); }
      }),
    );
  }
  body.append(danger);
}

// roleLabel maps a server role identifier to its localized label, falling back to
// the raw identifier for any role the UI doesn't know about.
function roleLabel(role) {
  return role === "admin" ? asI18n.t("admin.role.admin") : role === "user" ? asI18n.t("admin.role.user") : role;
}

function roleControl(u) {
  const sel = document.createElement("select");
  ["user", "admin"].forEach((r) => {
    const o = document.createElement("option");
    o.value = r; o.textContent = roleLabel(r);
    if (u.role === r) o.selected = true;
    sel.appendChild(o);
  });
  sel.addEventListener("change", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { role: sel.value });
      toast(asI18n.t("admin.toast.roleChanged", { role: roleLabel(sel.value) }));
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); sel.value = u.role; }
  });
  return sel;
}

function statusControl(u) {
  const wrap = div("inline");
  wrap.append(pill(u.disabled ? asI18n.t("admin.drawer.statusDisabled") : asI18n.t("admin.drawer.statusEnabled"), u.disabled ? "off" : "ok"));
  wrap.append(button(u.disabled ? asI18n.t("admin.drawer.enable") : asI18n.t("admin.drawer.disable"), "small", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { disabled: !u.disabled });
      toast(u.disabled ? asI18n.t("admin.toast.userEnabled") : asI18n.t("admin.toast.userDisabled"));
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); }
  }));
  return wrap;
}

function passwordControl(u) {
  const wrap = div("inline");
  wrap.append(pill(u.has_password ? asI18n.t("admin.drawer.passwordSet") : asI18n.t("admin.drawer.passwordNone"), u.has_password ? "ok" : "muted"));
  const inp = document.createElement("input");
  inp.type = "password";
  inp.placeholder = asI18n.t("admin.drawer.newPassword");
  inp.classList.add("hidden");
  const save = button(asI18n.t("admin.common.save"), "small", async () => {
    try {
      await api("PATCH", `/admin/users/${u.id}`, { password: inp.value });
      toast(asI18n.t("admin.toast.passwordUpdated"));
      await loadUsers();
      renderUserDrawer(u.id);
    } catch (err) { toast(err.message, "error"); }
  });
  save.classList.add("hidden");
  const setBtn = button(u.has_password ? asI18n.t("admin.drawer.changePassword") : asI18n.t("admin.drawer.setPassword"), "small", () => {
    inp.classList.toggle("hidden");
    save.classList.toggle("hidden");
    if (!inp.classList.contains("hidden")) inp.focus();
  });
  wrap.append(setBtn, inp, save);
  return wrap;
}

// Recovery codes are user-owned (minted from the player's own settings), so the
// admin view just reports whether the user can self-recover - plus a Revoke, the
// only lever to kill a leaked/compromised recovery code.
function recoveryControl(u) {
  const wrap = div("inline");
  wrap.append(pill(u.has_recovery ? asI18n.t("admin.drawer.recoverySet") : asI18n.t("admin.drawer.recoveryNone"), u.has_recovery ? "ok" : "muted"));
  if (u.has_recovery) {
    wrap.append(button(asI18n.t("admin.common.revoke"), "danger small", async () => {
      try { await api("DELETE", `/admin/users/${u.id}/recovery`); toast(asI18n.t("admin.toast.recoveryRevoked")); renderUserDrawer(u.id); }
      catch (err) { toast(err.message, "error"); }
    }));
  }
  return wrap;
}

// Invite state predicates. An invite is "active" (the one active invite per user,
// resendable) while it can still be redeemed - not expired and not used up.
// redeemed_at only records that it has been accepted at least once; a multi-use
// invite stays active until its uses run out. Spent/expired invites fall into
// History.
function codeExpired(c) { return !!c.expires_at && new Date(c.expires_at).getTime() <= Date.now(); }
function codeUsedUp(c) { return c.max_uses > 0 && c.uses >= c.max_uses; }
function codePending(c) { return !codeExpired(c) && !codeUsedUp(c); }

function statusPill(c) {
  if (codePending(c)) {
    if (!c.expires_at) return [asI18n.t("admin.code.active"), "ok"];
    const e = expiryLabel(c.expires_at);
    return [e.text, e.cls];
  }
  if (c.redeemed_at) return [asI18n.t("admin.code.accepted"), "ok"];
  if (codeExpired(c)) return [asI18n.t("admin.code.expired"), "off"];
  return [asI18n.t("admin.code.usedUp"), "muted"];
}

function codeCard(c, userId) {
  const card = div("codecard");
  const top = div("top");
  const label = document.createElement("strong");
  label.textContent = c.label || asI18n.t("admin.code.invite");
  const [text, cls] = statusPill(c);
  const tail = div("inline");
  tail.append(pill(text, cls));
  if (codePending(c)) {
    tail.append(button(asI18n.t("admin.code.resend"), "secondary small", async () => {
      try { showInviteResult(userId, await api("POST", `/admin/authcodes/${c.id}/rotate`, {})); }
      catch (err) { toast(err.message, "error"); }
    }));
  }
  tail.append(button(asI18n.t("admin.common.revoke"), "danger small", async () => {
    try { await api("DELETE", `/admin/authcodes/${c.id}`); toast(asI18n.t("admin.toast.codeRevoked")); renderUserDrawer(userId); }
    catch (err) { toast(err.message, "error"); }
  }));
  top.append(label, tail);
  card.append(top);
  const sub = div("sub");
  // "Accepted" is only meaningful for a spent/expired (history) invite; an active
  // invite shows when it was issued, with usesLabel conveying any partial use.
  const issued = !codePending(c) && c.redeemed_at
    ? asI18n.t("admin.code.acceptedAt", { time: fmtRelative(c.redeemed_at) })
    : asI18n.t("admin.code.issuedAt", { time: fmtRelative(c.created_at) });
  sub.append(span(issued), span(usesLabel(c)));
  card.append(sub);
  return card;
}

// historyDisclosure collapses spent/expired invites behind a toggle so the drawer
// stays clean while keeping an audit trail.
function historyDisclosure(history, userId) {
  const wrap = div();
  const items = div("hidden");
  history.forEach((c) => items.append(codeCard(c, userId)));
  const toggle = button(asI18n.t("admin.code.history", { count: history.length }), "small", () => items.classList.toggle("hidden"));
  wrap.append(spacerNode(), toggle, items);
  return wrap;
}

// grantRow renders one access grant (a named share or a whole-library sugar
// share) with its path chips and a revoke button.
function grantRow(s, userId) {
  const sc = div("codecard");
  const top = div("top");
  const nm = document.createElement("strong");
  nm.textContent = s.name;
  top.append(nm, button(asI18n.t("admin.common.revoke"), "danger small", async () => {
    try { await api("DELETE", "/admin/share-access", { user_id: userId, share_id: s.id }); toast(asI18n.t("admin.toast.accessRevoked")); await loadShares(); renderUserDrawer(userId); }
    catch (err) { toast(err.message, "error"); }
  }));
  sc.append(top);
  const chips = div("inline");
  (s.paths || []).forEach((r) => chips.append(chip(pathChipLabel(r))));
  if ((s.paths || []).length) sc.append(chips);
  return sc;
}

// accessAdder builds the "grant access" control: a kind dropdown (whole library
// or a named share) whose target select repopulates to match, plus a Grant button.
function accessAdder(userId) {
  const wrap = div("inline");
  const kind = document.createElement("select");
  [["library", asI18n.t("admin.access.wholeLibrary")], ["share", asI18n.t("admin.access.share")]].forEach(([v, l]) => {
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
  const grant = button(asI18n.t("admin.access.grant"), "small", async () => {
    if (!target.value) return;
    try {
      if (kind.value === "library") await api("POST", "/admin/library-access", { user_id: userId, library_id: Number(target.value) });
      else await api("POST", "/admin/share-access", { user_id: userId, share_id: Number(target.value) });
      toast(asI18n.t("admin.toast.accessGranted"));
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
  if (maxUses === null || ttlDays === null) { toast(asI18n.t("admin.invite.customError"), "error"); return; }
  try {
    const data = await api("POST", `/admin/users/${inviteUserId}/authcode`,
      { label: "invite", max_uses: maxUses, ttl_days: ttlDays });
    showInviteResult(inviteUserId, data);
  } catch (err) { toast(err.message, "error"); }
});

// Show the invite modal's result panel with a freshly minted or rotated code +
// link (returned once - this is the only chance to copy them). Shared by Create
// invite and Resend; the latter opens the modal straight to the result.
function showInviteResult(userId, data) {
  inviteUserId = userId;
  el("inv-link").textContent = data.invite_url;
  el("inv-code").textContent = data.auth_code;
  el("invite-form").classList.add("hidden");
  el("invite-result").classList.remove("hidden");
  openModal("modal-invite");
}

async function copyField(text, label) {
  if (await copyToClipboard(text)) toast(asI18n.t("admin.toast.copied", { label }));
  else toast(asI18n.t("admin.toast.copyManual", { label, text }));
}
el("inv-copy-link").addEventListener("click", () => copyField(el("inv-link").textContent, asI18n.t("admin.invite.linkLabel")));
el("inv-copy-code").addEventListener("click", () => copyField(el("inv-code").textContent, asI18n.t("admin.invite.codeLabel")));
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
// editingShareId is null in create mode, or the id of the share being renamed.
let editingShareId = null;

el("add-share-btn").addEventListener("click", () => openShareModal(null));

// openShareModal drives the shared modal for both creating and renaming: edit mode
// prefills the name and switches the title/submit copy. The same modal-share form
// submits to POST (create) or PATCH (rename) accordingly.
function openShareModal(share) {
  editingShareId = share ? share.id : null;
  el("s-name").value = share ? share.name : "";
  el("share-modal-title").textContent = asI18n.t(share ? "admin.shareModal.editTitle" : "admin.shareModal.title");
  el("share-submit").textContent = asI18n.t(share ? "admin.shareModal.editSubmit" : "admin.shareModal.submit");
  el("share-modal-help").classList.toggle("hidden", !!share); // the "then add paths" hint is create-only
  openModal("modal-share");
}

el("share-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = el("s-name").value.trim();
  try {
    if (editingShareId !== null) {
      await api("PATCH", `/admin/shares/${editingShareId}`, { name });
      toast(asI18n.t("admin.toast.shareUpdated"));
    } else {
      await api("POST", "/admin/shares", { name });
      toast(asI18n.t("admin.toast.shareCreated"));
    }
    el("share-form").reset();
    editingShareId = null;
    closeModals();
    await loadShares();
  } catch (err) { toast(err.message, "error"); }
});

async function loadShares() {
  const { shares } = await api("GET", "/admin/shares");
  sharesCache = shares || [];
  const list = el("share-list");
  list.innerHTML = "";
  if (!sharesCache.length) { list.append(card(emptyNote(asI18n.t("admin.shares.empty")))); return; }
  sharesCache.forEach((s) => list.appendChild(shareCard(s)));
}

function libName(id) {
  const l = librariesCache.find((x) => x.id === id);
  return l ? l.name : asI18n.t("admin.shares.libFallback", { id });
}

// pathChipLabel renders one share path rule as "Library › sub/path" or
// "Library (whole library)" for the empty ("") whole-library rule.
function pathChipLabel(r) {
  return r.path ? libName(r.library_id) + " › " + r.path : asI18n.t("admin.shares.wholeLibraryChip", { library: libName(r.library_id) });
}

function shareCard(s) {
  const c = card();
  const head = div("page-head");
  const title = document.createElement("h2");
  title.textContent = s.name;
  const actions = div("inline");
  actions.append(
    button(asI18n.t("admin.shares.editShare"), "ghost small", () => openShareModal(s)),
    deleteShareBtn(s),
  );
  head.append(title, actions);
  c.appendChild(head);

  const chips = div("inline");
  (s.paths || []).forEach((r) => {
    const ch = document.createElement("span");
    ch.className = "chip";
    ch.append(document.createTextNode(pathChipLabel(r)));
    const x = document.createElement("span");
    x.className = "x";
    x.textContent = "✕";
    x.title = asI18n.t("admin.shares.removePath");
    x.addEventListener("click", async () => {
      try { await api("DELETE", `/admin/shares/${s.id}/paths`, { library_id: r.library_id, path: r.path }); await loadShares(); }
      catch (err) { toast(err.message, "error"); }
    });
    ch.append(x);
    chips.appendChild(ch);
  });
  if (!(s.paths || []).length) chips.append(emptyNote(asI18n.t("admin.shares.noPaths")));
  c.appendChild(chips);

  const controls = div("inline");
  controls.append(button(asI18n.t("admin.shares.browseAdd"), "secondary small", () => openPicker(s)));
  c.appendChild(spacerNode());
  c.appendChild(controls);
  return c;
}

function deleteShareBtn(s) {
  return button(asI18n.t("admin.shares.deleteShare"), "danger small", async () => {
    if (!confirm(asI18n.t("admin.confirm.deleteShare", { name: s.name }))) return;
    try { await api("DELETE", `/admin/shares/${s.id}`); toast(asI18n.t("admin.toast.shareDeleted")); await loadShares(); }
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
    toast(path ? asI18n.t("admin.toast.pathAdded", { path }) : asI18n.t("admin.toast.wholeLibraryAdded"));
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
  em.textContent = path ? asI18n.t("admin.picker.thisFolder") : asI18n.t("admin.picker.wholeLibrary");
  hereLabel.append(em);
  here.append(hereLabel, actionTd(button(path ? asI18n.t("admin.picker.shareThisFolder") : asI18n.t("admin.picker.shareWholeLibrary"), "small", () => addPickedPath(path))));
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
      ? actionTd(button(asI18n.t("admin.picker.shareThis"), "small", () => addPickedPath(entry.path)))
      : document.createElement("td");
    tr.append(name, action);
    rows.appendChild(tr);
  });
}

// ---- Folder detection overrides (modal) ----
let detectLib = null;

function openDetect(lib) {
  detectLib = lib.id;
  el("detect-lib").textContent = lib.name;
  openModal("modal-detect");
  detectNavigate("");
}

// DETECT_MODES pairs each override value with its i18n key; the label is resolved
// at render time (so a language switch re-applies on the next open).
const DETECT_MODES = [["", "admin.detect.auto"], ["book", "admin.detect.oneBook"], ["collection", "admin.detect.separateBooks"]];

async function detectNavigate(path) {
  el("detect-crumb").textContent = "/" + path;
  const rows = el("detect-rows");
  rows.innerHTML = "";

  let listing;
  try { listing = await api("GET", `/libraries/${detectLib}/fs?path=${encodeURIComponent(path)}&limit=500`); }
  catch (err) { toast(err.message, "error"); return; }

  if (path) {
    const up = document.createElement("tr");
    const upName = document.createElement("td");
    const parent = path.split("/").slice(0, -1).join("/");
    const a = document.createElement("a");
    a.href = "#";
    a.textContent = "⬆ ..";
    a.addEventListener("click", (e) => { e.preventDefault(); detectNavigate(parent); });
    upName.append(a);
    up.append(upName, document.createElement("td"));
    rows.appendChild(up);
  }

  (listing.entries || []).forEach((entry) => {
    if (!entry.is_dir) return; // overrides apply to folders
    const tr = document.createElement("tr");
    const name = document.createElement("td");
    const a = document.createElement("a");
    a.href = "#";
    a.textContent = "📁 " + entry.name + (entry.is_book ? asI18n.t("admin.detect.bookSuffix") : "");
    a.addEventListener("click", (e) => { e.preventDefault(); detectNavigate(entry.path); });
    name.appendChild(a);

    const sel = document.createElement("select");
    DETECT_MODES.forEach(([value, labelKey]) => {
      const o = document.createElement("option");
      o.value = value;
      o.textContent = asI18n.t(labelKey);
      if ((entry.override || "") === value) o.selected = true;
      sel.appendChild(o);
    });
    sel.addEventListener("change", async () => {
      const q = `?path=${encodeURIComponent(entry.path)}`;
      try {
        if (sel.value === "") await api("DELETE", `/admin/libraries/${detectLib}/folder-override${q}`);
        else await api("PUT", `/admin/libraries/${detectLib}/folder-override${q}`, { mode: sel.value });
        entry.override = sel.value;
        toast(asI18n.t("admin.toast.detectionUpdated"));
      } catch (err) { toast(err.message, "error"); sel.value = entry.override || ""; }
    });
    const action = document.createElement("td");
    action.appendChild(sel);
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
  if (!ts) return asI18n.t("admin.time.never");
  const then = new Date(ts).getTime();
  if (isNaN(then)) return "-";
  const s = Math.floor((Date.now() - then) / 1000);
  if (s < 45) return asI18n.t("admin.time.justNow");
  if (s < 3600) return asI18n.t("admin.time.minutesAgo", { n: Math.floor(s / 60) });
  if (s < 86400) return asI18n.t("admin.time.hoursAgo", { n: Math.floor(s / 3600) });
  if (s < 86400 * 30) return asI18n.t("admin.time.daysAgo", { n: Math.floor(s / 86400) });
  return new Date(ts).toLocaleDateString();
}

function expiryLabel(expiresAt) {
  if (!expiresAt) return { text: asI18n.t("admin.code.noExpiry"), cls: "muted" };
  const ms = new Date(expiresAt).getTime() - Date.now();
  if (ms <= 0) return { text: asI18n.t("admin.code.expired"), cls: "off" };
  return { text: asI18n.t("admin.code.timeLeft", { dur: shortDur(ms) }), cls: "ok" };
}
function shortDur(ms) {
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return asI18n.t("admin.dur.dh", { d, h });
  if (h > 0) return asI18n.t("admin.dur.hm", { h, m });
  if (m > 0) return asI18n.t("admin.dur.m", { m });
  return asI18n.t("admin.dur.s", { s });
}
function usesLabel(c) {
  if (c.max_uses === 0) return c.uses > 0 ? asI18n.t("admin.uses.unlimitedUsed", { n: c.uses }) : asI18n.t("admin.uses.unlimitedUnused");
  const usedUp = c.uses >= c.max_uses;
  return usedUp ? asI18n.t("admin.uses.usedUp", { used: c.uses, max: c.max_uses }) : asI18n.t("admin.uses.used", { used: c.uses, max: c.max_uses });
}

// ---- Boot ----
// Show the running server's version in the sidebar. /server is public, so this
// works regardless of auth state and never blocks the dashboard.
async function loadServerVersion() {
  try {
    const info = await api("GET", "/server");
    if (info.version) el("server-version").textContent = asI18n.t("admin.foot.serverVersion", { version: info.version });
  } catch (_) { /* non-fatal: just leave the version blank */ }
}

(async function boot() {
  loadServerVersion();
  if (!token) return; // show login
  try {
    const me = await api("GET", "/me");
    if (me.role !== "admin") { logout(); return; }
    myUserId = me.id;
    el("who").textContent = me.username;
    await enterDashboard();
  } catch (_) { /* api() already handled 401 by logging out */ }
})();

// ---- PWA ----
// Register the service worker so the admin console is installable and the shell
// works offline. Service workers require a secure context, so this is a no-op
// over plain http on a LAN IP - it works on localhost or trusted HTTPS, which is
// the normal case for a locally-run server. The browser provides its own install
// affordance; we add no in-app button.
if ("serviceWorker" in navigator && window.isSecureContext) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => { /* non-fatal */ });
  });
}
