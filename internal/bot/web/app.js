"use strict";
const $ = id => document.getElementById(id);
const enc = encodeURIComponent;
// The areas of the side menu, with their views. The view names are unique.
const AREAS = {
  comics: {title: "Comics", views: [["overview", "Overview"], ["characters", "Characters"], ["backgrounds", "Backgrounds"], ["members", "Members"], ["preview", "Preview"]]},
  speech: {title: "Speech", views: [["send", "Send"], ["clips", "Clips"], ["voices", "Voices"]]},
  markov: {title: "Markov", views: [["markov", "Generate"]]},
  persona: {title: "Persona", views: [["persona", "Generate"]]},
  tickets: {title: "Tickets", views: [["tickets", "Tickets"]]},
  settings: {title: "Settings", views: [["settings", "Settings"]]},
};
const VIEWS = Object.values(AREAS).flatMap(a => a.views.map(v => v[0]));
const areaOf = view => Object.keys(AREAS).find(a => AREAS[a].views.some(v => v[0] === view));
let me = null, art = null, members = null;
// Where the page is: the area, the view, and the open item ("+" is the add form).
let here = {area: "", view: "", item: ""};
// Members that a search found, by ID, for the details panel.
const known = {};

// el builds an element. All text goes in as text, not as HTML.
function el(tag, props = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k in n && k !== "href") n[k] = v;
    else n.setAttribute(k, v);
  }
  n.append(...kids);
  return n;
}

function dialogOpen() { return $("dialog").open; }

// A message goes next to the work: in the confirmation or the details panel
// if one is open. It stays there until the next action. The toast is for the
// rest. kind is "" (done), "warn" (done, but look at this), or "bad" (failed).
function toast(text, kind, wait) {
  if (kind === true) kind = "bad";
  const inner = dialogOpen() ? $("dialog-msg") : (!$("panel").hidden ? $("panel-msg") : null);
  if (inner) {
    inner.textContent = text; inner.className = "msg " + (kind || "");
    inner.scrollIntoView({block: "nearest"});
    return;
  }
  const t = $("toast");
  t.textContent = text; t.className = kind || "";
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.textContent = ""; t.className = ""; }, wait || (kind ? 10000 : 4000));
}

function clearMessages() {
  for (const id of ["dialog-msg", "panel-msg"]) { $(id).textContent = ""; $(id).className = "msg"; }
}

let pending = 0;
function working(by) {
  pending += by;
  $("busy").hidden = pending === 0;
}

async function api(method, path, body) {
  const opt = {method, headers: {}};
  if (body instanceof FormData) opt.body = body;
  else if (body !== undefined) { opt.body = JSON.stringify(body); opt.headers["Content-Type"] = "application/json"; }
  working(1);
  try {
    let r;
    try {
      r = await fetch(path, opt);
    } catch {
      throw new Error("I could not reach the bot. Check the connection and try again.");
    }
    if (r.status === 401) { showLogin(me ? "Your login ended. Log in again." : undefined); throw new Error("Please log in."); }
    const text = await r.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
    if (!r.ok) throw new Error((data && data.error) || "The bot could not do this (error " + r.status + "). Try again.");
    return data;
  } finally {
    working(-1);
  }
}

// act runs a change and shows its result. A change to the art answers with
// the new art. A warning shows with the success text, not in its place: the
// change worked. button, if given, cannot be clicked again during the change.
async function act(run, done, button) {
  clearMessages();
  if (button) button.disabled = true;
  try {
    const data = await run();
    if (data && data.characters) { art = data; renderAll(); }
    const text = [done, data && data.warning].filter(Boolean).join(" ");
    if (text) toast(text, data && data.warning ? "warn" : "");
    return data || true;
  } catch (e) {
    toast(e.message, true);
    return null;
  } finally {
    if (button) button.disabled = false;
  }
}

// keepFocus restores the focus after a part of the page is drawn again.
// Elements that can have the focus there have a data-key.
function keepFocus(draw) {
  const key = document.activeElement && document.activeElement.dataset && document.activeElement.dataset.key;
  draw();
  focusKey(key);
}
function focusKey(key) {
  if (!key) return;
  // A mod has no Replace button after an upload, only the undo.
  const find = k => document.querySelector('[data-key="' + CSS.escape(k) + '"]');
  const n = find(key) || find(key + ":remove");
  if (!n) return;
  // A new component has no inner button until its first draw.
  if (n.updateComplete) n.updateComplete.then(() => n.focus());
  else n.focus();
}

function showLogin(text) {
  $("loading").hidden = true;
  $("shell").hidden = true; $("boot-error").hidden = true; $("login").hidden = false;
  if (dialogOpen()) $("dialog").close();
  if (text) $("login-text").textContent = text;
}

function pickFile(accept) {
  return new Promise(resolve => {
    const f = $("file");
    f.value = ""; f.accept = accept;
    f.onchange = () => resolve(f.files[0] || null);
    f.oncancel = () => resolve(null);
    f.click();
  });
}

function form(file, fields = {}) {
  const fd = new FormData();
  fd.append("file", file);
  for (const [k, v] of Object.entries(fields)) fd.append(k, v);
  return fd;
}

// confirmDanger asks before a destructive action. run returns a true value
// when it worked. If it fails, the dialog stays open with the message.
function confirmDanger(title, text, label, run) {
  $("dialog-title").textContent = title;
  $("dialog-body").replaceChildren(el("p", {class: "md-typescale-body-medium"}, text));
  clearMessages();
  const cancel = el("md-text-button", {onclick: () => $("dialog").close()}, "Cancel");
  const go = el("md-filled-button", {class: "danger", onclick: async () => {
    cancel.disabled = go.disabled = true; // no second click while it runs
    const ok = await run();
    cancel.disabled = go.disabled = false;
    if (ok) $("dialog").close();
  }}, label);
  $("dialog-actions").replaceChildren(cancel, go);
  $("dialog").show();
}

const TRASH = "An operator can restore it from the trash folder.";

// undoLeft is the text for the time that is left to undo an upload.
function undoLeft(key) {
  const minutes = Math.max(1, Math.ceil((art.undo[key] || 0) / 60));
  return "You can undo your upload for " + minutes + (minutes === 1 ? " more minute." : " more minutes.");
}

// ---- Links: each view and each open item has its own address. ----

let keepMessage = false;
let focusSearch = false; // "Find member" goes to the search field
function go(view, item) {
  location.hash = "#/" + areaOf(view) + "/" + view + (item ? "/" + enc(item) : "");
}

function readHash() {
  const parts = location.hash.replace(/^#\/?/, "").split("/");
  const area = AREAS[parts[0]] ? parts[0] : "comics";
  const views = AREAS[area].views.map(v => v[0]);
  const view = views.includes(parts[1]) ? parts[1] : views[0];
  let item = "";
  try { item = parts[2] ? decodeURIComponent(parts[2]) : ""; } catch { /* a damaged link: no item */ }
  return {area, view, item};
}

// A new tab bar for each area: md-tabs does not follow a change of its tabs.
function renderTabs(area, view) {
  const views = AREAS[area].views;
  const tabs = el("md-tabs", {id: "tabs", ariaLabel: "Views", onchange: () => {
    const active = tabs.activeTab;
    const id = active && active.getAttribute("aria-controls");
    if (id && id !== here.view) go(id);
  }}, ...(views.length < 2 ? [] : views.map(([id, label]) =>
    el("md-primary-tab", {id: "tab-" + id, "aria-controls": id, active: id === view}, label))));
  tabs.hidden = views.length < 2;
  $("tabs").replaceWith(tabs);
}

function route() {
  const next = readHash();
  const changed = next.view !== here.view || next.item !== here.item || next.area !== here.area;
  // The result of an add must stay when the new item opens. A message in a
  // confirmation stays until the confirmation closes.
  if (changed && !keepMessage) { $("panel-msg").textContent = ""; $("panel-msg").className = "msg"; }
  keepMessage = false;
  const newArea = next.area !== here.area;
  here = next;
  closeMenu();
  if (newArea) {
    renderTabs(here.area, here.view);
    $("area-title").textContent = AREAS[here.area].title;
    for (const a of Object.keys(AREAS)) {
      if (a === here.area) $("nav-" + a).setAttribute("aria-current", "page");
      else $("nav-" + a).removeAttribute("aria-current");
    }
    renderCounts();
  }
  const tab = AREAS[here.area].views.findIndex(v => v[0] === here.view);
  if (AREAS[here.area].views.length > 1 && $("tabs").activeTabIndex !== tab) $("tabs").activeTabIndex = tab;
  const label = AREAS[here.area].views[tab][1];
  const pageTitle = AREAS[here.area].views.length > 1 ? label : AREAS[here.area].title;
  document.title = (here.item && here.item !== "+" && here.view !== "preview" ? here.item.split("/").pop() + " · " : "") + pageTitle + " · HEALTH Bot";
  const why = me.off && me.off[here.area];
  $("off").hidden = !why;
  $("off-text").textContent = why || "";
  $("tabs").hidden = !!why || AREAS[here.area].views.length < 2;
  for (const v of VIEWS) $(v).hidden = !!why || v !== here.view;
  if (why) { $("panel").hidden = true; syncCovered(); return; }
  if (here.view === "overview" || here.view === "members") loadMembers();
  if (here.area === "speech") loadSpeech();
  if (here.view === "clips") loadClipDays();
  if (here.view === "settings") loadSettings();
  if (here.view === "tickets" && !tickets.list) loadTickets();
  renderView();
  if (here.view === "preview" && here.item) presetPreview(here.item);
  renderPanel();
  syncCovered();
  if (focusSearch) { focusSearch = false; $("member-q").focus(); }
  if (changed && !$("panel").hidden) {
    const field = here.item === "+" && $("panel-body").querySelector("md-outlined-text-field");
    if (field) field.updateComplete.then(() => field.focus()); // it has no input until its first draw
    else $("panel-title").focus();
  }
}

function closePanel() {
  const {view, item} = here;
  go(view === "preview" ? "preview" : view);
  // The focus returns to the item in the list.
  setTimeout(() => focusKey("open:" + view + ":" + item), 0);
}

// ---- The views. ----

function renderAll() {
  keepFocus(() => { renderCounts(); renderView(); renderPanel(); });
}

function renderCounts() {
  if (!art) return;
  const n = (art.problems || []).length;
  $("nav-count").hidden = n === 0;
  $("nav-count").textContent = n;
  $("nav-count").setAttribute("aria-label", n + (n === 1 ? " art problem" : " art problems"));
  if ($("tab-overview")) $("tab-overview").textContent = n ? "Overview (" + n + ")" : "Overview";
}

function renderView() {
  if (here.view === "overview") renderOverview();
  if (here.view === "characters") renderCharacters();
  if (here.view === "backgrounds") renderBackgrounds();
  if (here.view === "members") renderMembers();
  // The options change only with the art. Drawing them again resets a choice
  // that the user just made in the open list.
  if (here.view === "preview" && previewArt !== art) { previewArt = art; renderPreviewOptions(); }
  if (here.view === "send") renderSend();
  if (here.view === "clips") renderClips();
  if (here.view === "voices") renderVoices();
  if (here.view === "markov" || here.view === "persona") renderChat(here.view);
  if (here.view === "settings") renderSettings();
  if (here.view === "tickets") renderTickets();
}

let previewArt = null;

function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }

function renderOverview() {
  const chars = art.characters, backs = art.backgrounds;
  const parts = (list, withPrivate) => {
    const bits = [];
    const custom = list.filter(x => x.custom).length, hidden = list.filter(x => x.hidden).length;
    if (custom) bits.push(custom + " custom");
    if (withPrivate) {
      const priv = list.filter(x => x.users && x.users.length).length;
      if (priv) bits.push(priv + " private");
    }
    if (hidden) bits.push(hidden + " hidden");
    return bits.length ? " (" + bits.join(", ") + ")" : "";
  };
  const rows = [
    ["Characters", "characters", plural(chars.length, "character", "characters") + parts(chars, true)],
    ["Backgrounds", "backgrounds", plural(backs.length, "background", "backgrounds") + parts(backs, false)],
  ];
  if (members) {
    const locked = members.filter(m => m.locked).length;
    rows.push(["Members", "members", plural(members.length, "member has a character", "members have a character") + (locked ? " (" + locked + " locked)" : "")]);
  }
  $("facts").replaceChildren(...rows.flatMap(([label, view, text]) => [
    el("dt", {}, label), el("dd", {}, el("a", {href: "#/comics/" + view}, text))]));
  const list = art.problems || [];
  $("problem-list").replaceChildren(...(list.length
    ? [el("p", {class: "hint md-typescale-body-small", style: "margin-bottom:8px"}, "Comics do not use these files. An operator must correct or remove them in the art folder."),
       ...list.map(p => el("div", {class: "problem md-typescale-body-medium"}, p))]
    : [el("p", {class: "hint md-typescale-body-medium"}, "All the art files work.")]));
}

// Filters stay when the view is drawn again.
const filters = {
  characters: {status: "all"},
  backgrounds: {status: "all"},
};

function renderChips(host, view, options) {
  const f = filters[view];
  keepFocus(() => host.replaceChildren(...options.map(([value, label, count]) =>
    el("md-filter-chip", {label: label + " " + count, selected: f.status === value, "data-key": "chip:" + view + ":" + value, onclick: () => {
      f.status = value;
      renderView();
      focusKey("chip:" + view + ":" + value);
    }}))));
}

function emptyResult(host, view, input) {
  host.replaceChildren(el("div", {class: "empty", style: "grid-column:1/-1"},
    el("p", {class: "hint md-typescale-body-medium"}, "Nothing matches this filter."),
    el("md-outlined-button", {onclick: () => { filters[view].status = "all"; input.value = ""; renderView(); input.focus(); }}, "Clear the filter")));
}

// Custom art first: mods change it most. Then the usual order.
const customFirst = list => [...list].sort((a, b) => (b.custom ? 1 : 0) - (a.custom ? 1 : 0));

function renderCharacters() {
  const chars = customFirst(art.characters);
  const tests = {
    all: () => true, custom: c => c.custom, hidden: c => c.hidden,
    used: c => art.used[c.name] > 0, private: c => c.users && c.users.length > 0,
  };
  renderChips($("char-chips"), "characters", [
    ["all", "All", chars.length], ["custom", "Custom", chars.filter(tests.custom).length],
    ["hidden", "Hidden", chars.filter(tests.hidden).length], ["used", "In use", chars.filter(tests.used).length],
    ["private", "Private", chars.filter(tests.private).length],
  ]);
  const q = $("char-filter").value.trim().toLowerCase();
  const list = chars.filter(c => c.name.includes(q) && tests[filters.characters.status](c));
  if (!list.length) { emptyResult($("char-grid"), "characters", $("char-filter")); return; }
  keepFocus(() => $("char-grid").replaceChildren(...list.map(c => {
    const used = art.used[c.name] || 0;
    const tags = el("span", {class: "tags"});
    if (c.custom) tags.append(el("span", {class: "tag"}, "custom"));
    if (c.users && c.users.length) tags.append(el("span", {class: "tag limit"}, "private"));
    if (used) tags.append(el("span", {class: "tag use"}, plural(used, "member", "members")));
    if (c.hidden) tags.append(el("span", {class: "tag off"}, "hidden"));
    const open = here.view === "characters" && here.item === c.name;
    const a = el("a", {class: "tile" + (c.hidden ? " off" : ""), href: "#/comics/characters/" + enc(c.name), "data-key": "open:characters:" + c.name},
      el("img", {class: "thumb", src: "/img/icon/" + enc(c.name), alt: "", loading: "lazy"}),
      el("span", {class: "name md-typescale-label-large"}, c.name), tags);
    if (open) a.setAttribute("aria-current", "true");
    return a;
  })));
}

function renderBackgrounds() {
  const backs = customFirst(art.backgrounds);
  const tests = {all: () => true, custom: b => b.custom, hidden: b => b.hidden};
  renderChips($("back-chips"), "backgrounds", [
    ["all", "All", backs.length], ["custom", "Custom", backs.filter(tests.custom).length],
    ["hidden", "Hidden", backs.filter(tests.hidden).length],
  ]);
  const q = $("back-filter").value.trim().toLowerCase();
  const list = backs.filter(b => b.name.includes(q) && tests[filters.backgrounds.status](b));
  if (!list.length) { emptyResult($("back-grid"), "backgrounds", $("back-filter")); return; }
  keepFocus(() => $("back-grid").replaceChildren(...list.map(b => {
    const tags = el("span", {class: "tags"});
    if (b.custom) tags.append(el("span", {class: "tag"}, "custom"));
    if (b.hidden) tags.append(el("span", {class: "tag off"}, "hidden"));
    const a = el("a", {class: "tile" + (b.hidden ? " off" : ""), href: "#/comics/backgrounds/" + enc(b.name), "data-key": "open:backgrounds:" + b.name},
      el("img", {class: "thumb back", src: "/img/background/" + enc(b.name), alt: "", loading: "lazy"}),
      el("span", {class: "name md-typescale-label-large"}, b.name), tags);
    if (here.view === "backgrounds" && here.item === b.name) a.setAttribute("aria-current", "true");
    return a;
  })));
}

function memberRow(m) {
  known[m.id] = m;
  const row = el("div", {class: "member"},
    m.avatar ? el("img", {src: m.avatar, alt: ""}) : el("div", {class: "noavatar"}),
    el("div", {class: "who"}, el("div", {class: "md-typescale-body-large"}, m.name || "(name not found)"),
      el("div", {class: "id label md-typescale-body-small"}, m.id)));
  if (m.character) {
    const has = el("div", {class: "has"},
      el("img", {src: "/img/icon/" + enc(m.character), alt: "", loading: "lazy"}),
      el("span", {class: "md-typescale-label-large"}, m.character));
    if (m.locked) has.append(el("span", {class: "tag limit"}, "locked"));
    row.append(has);
  }
  row.append(el("md-outlined-button", {"data-key": "open:members:" + m.id, href: "#/comics/members/" + m.id,
    ariaLabel: (m.character ? "Change " : "Assign ") + (m.name || m.id)}, m.character ? "Change" : "Assign"));
  return row;
}

async function loadMembers() {
  const list = await api("GET", "/api/members").catch(e => { toast(e.message, true); return null; });
  if (!list) return;
  members = list;
  for (const m of list) known[m.id] = m;
  renderView();
  if (here.view === "members" && here.item) renderPanel();
}

function renderMembers() {
  if (!members) {
    $("member-list").replaceChildren(el("p", {class: "hint md-typescale-body-medium"}, "Loading members…"));
    return;
  }
  const q = $("member-filter").value.trim().toLowerCase();
  const list = members.filter(m => [m.name, m.id, m.character].some(v => (v || "").toLowerCase().includes(q)));
  if (!list.length && members.length) {
    $("member-list").replaceChildren(el("div", {class: "empty"},
      el("p", {class: "hint md-typescale-body-medium"}, "Nothing matches this filter."),
      el("md-outlined-button", {onclick: () => { $("member-filter").value = ""; renderMembers(); $("member-filter").focus(); }}, "Clear the filter")));
    return;
  }
  keepFocus(() => $("member-list").replaceChildren(...(list.length ? list.map(memberRow)
    : [el("p", {class: "hint md-typescale-body-medium"}, "No member has a character yet. Search above to assign one.")])));
}

async function findMember() {
  const q = $("member-q").value.trim();
  if (!q) return;
  const btn = $("member-find");
  btn.disabled = true;
  const found = await api("GET", "/api/members/search?q=" + enc(q)).catch(e => { toast(e.message, true); return null; });
  btn.disabled = false;
  if (!found) return;
  $("member-found").replaceChildren(...(found.length ? found.map(memberRow)
    : [el("p", {class: "hint md-typescale-body-medium"}, "I found no member with that name or ID. Check the text, or paste the user ID.")]));
}

// setOptions fills a select. It keeps the old value, or takes the first
// option, unless noDefault: an empty select means that nothing is chosen yet.
function setOptions(select, options, noDefault, chosen) {
  const old = chosen !== undefined ? chosen : select.value;
  const value = options.some(o => o[0] === old) ? old : (noDefault ? "" : options[0][0]);
  select.replaceChildren(...options.map(([v, text]) =>
    el("md-select-option", {value: v}, el("div", {slot: "headline"}, text))));
  select.value = value;
  // The select knows new options only after they are drawn. Before that, it has
  // the value but shows no text.
  requestAnimationFrame(() => requestAnimationFrame(() => { if (value) select.select(value); }));
}

function renderPreviewOptions(character) {
  setOptions($("prev-char"), [["*", "At random"], ...art.characters.map(c => [c.name, c.name])], false, character);
  setOptions($("prev-back"), [["*", "At random"], ...art.backgrounds.map(b => [b.name, b.name])]);
}

function presetPreview(name) {
  renderPreviewOptions(name);
  drawPreview();
}

function drawPreview() {
  const pick = s => s.value === "*" ? "" : s.value;
  const q = new URLSearchParams({character: pick($("prev-char")), background: pick($("prev-back")), seed: Math.floor(Math.random() * 1e9)});
  const img = $("preview-img"), btn = $("prev-draw");
  btn.disabled = true;
  working(1);
  const done = () => { btn.disabled = false; working(-1); img.onload = img.onerror = null; };
  img.onload = () => { img.hidden = false; done(); };
  img.onerror = () => { done(); toast("I could not draw the comic. Try again.", true); };
  img.src = "/api/preview?" + q;
}

// ---- The details panel. ----

// The private list that is not saved yet, by character. It must not be lost
// when the panel is drawn again after a pose upload.
const usersDraft = {};

function renderPanel() {
  const {view, item} = here;
  const show = (title, body) => {
    $("panel-title").textContent = title;
    $("panel-body").replaceChildren(...body);
    $("panel").hidden = false;
  };
  if (!item || view === "overview" || view === "preview" || view === "send" || view === "markov" || view === "persona") { $("panel").hidden = true; return; }
  if (view === "tickets") {
    const d = ticketDetail[item];
    if (d) { show("Ticket " + d.ref, ticketBody(d)); return; }
    if (/^[tl][0-9]{1,18}$/.test(item)) {
      show("Ticket", [el("p", {class: "hint md-typescale-body-medium"}, "Loading the transcript…")]);
      api("GET", "/api/tickets/" + enc(item)).then(data => {
        ticketDetail[item] = data;
        if (here.item === item) renderPanel();
      }).catch(e => toast(e.message, true));
      return;
    }
  }
  if (view === "settings") {
    if (!settings) { $("panel").hidden = true; return; } // it opens when they load
    const k = settings.sections.flatMap(sec => sec.settings).find(x => x.key === item);
    if (k) { show(k.key, settingBody(k)); return; }
  }
  if (view === "voices") {
    if (!speech) { $("panel").hidden = true; return; } // it opens when they load
    if (item === "+" && me.admin) { show("Add a voice", addVoiceBody()); return; }
  }
  if (view === "clips") {
    const slash = item.indexOf("/");
    const day = item.slice(0, slash), file = item.slice(slash + 1);
    if (clipData.day !== day) {
      show(file, [el("p", {class: "hint md-typescale-body-medium"}, "Loading the clip…")]);
      loadClips(day).then(() => { if (here.item === item) { renderClips(); renderPanel(); } });
      return;
    }
    const clip = clipData.clips.find(c => c.file === file);
    if (clip && speech) { show(clip.voice + " clip", clipBody(day, clip)); return; }
    if (clip) { show(clip.voice + " clip", [el("p", {class: "hint md-typescale-body-medium"}, "Loading…")]); return; }
  }
  if (item === "+" && view === "characters") { show("Add a character", addCharacterBody()); return; }
  if (item === "+" && view === "backgrounds") { show("Add a background", addBackgroundBody()); return; }
  if (view === "characters") {
    const c = art.characters.find(x => x.name === item);
    if (c) { show(c.name, characterBody(c)); return; }
  }
  if (view === "backgrounds") {
    const b = art.backgrounds.find(x => x.name === item);
    if (b) { show(b.name, backgroundBody(b)); return; }
  }
  if (view === "members" && /^[0-9]{1,20}$/.test(item)) {
    if (known[item]) { show(known[item].name || item, memberBody(known[item])); return; }
    // A link to a member who is not loaded yet.
    show(item, [el("p", {class: "hint md-typescale-body-medium"}, "Finding this member…")]);
    api("GET", "/api/members/search?q=" + enc(item)).then(found => {
      known[item] = found[0] || {id: item};
      if (here.item === item) renderPanel();
    }).catch(e => toast(e.message, true));
    return;
  }
  // The item is gone, for example after a removal.
  $("panel").hidden = true;
  go(view);
}

function visibleSwitch(kind, item) {
  const key = "hide:" + kind + ":" + item.name;
  const s = el("md-switch", {selected: !item.hidden, ariaLabel: "Visible: " + item.name, "data-key": key, onchange: async () => {
    const hidden = !s.selected;
    const ok = await act(() => api("POST", "/api/hidden", {kind, name: item.name, hidden}),
      item.name + (hidden ? " is hidden." : " is visible."), s);
    if (!ok) s.selected = !s.selected;
    focusKey(key);
  }});
  return el("label", {class: "row md-typescale-body-medium"}, "Visible", s);
}

// nameField is the name of new art. Enter opens the file picker.
function nameField(label, onEnter) {
  return el("md-outlined-text-field", {label, maxLength: 32, class: "wide", supportingText: "1 to 32 of a–z, 0–9, - and _. It starts with a letter or number.",
    onkeydown: e => { if (e.key === "Enter") onEnter(); }});
}

const MOD_NOTE = "You can undo your own upload for 15 minutes. After that, only an Administrator can replace or remove it.";

function addCharacterBody() {
  const add = async () => {
    const value = name.value.trim().toLowerCase();
    if (!nameOK(value)) return;
    const f = await pickFile("image/png");
    if (f && await act(() => api("POST", "/api/characters", form(f, {name: value})), "Added.", pick)) { keepMessage = true; go("characters", value); }
  };
  const name = nameField("Name", add);
  const pick = el("md-filled-button", {onclick: add}, "Select a PNG file");
  const body = [name,
    el("p", {class: "hint md-typescale-body-small"}, "The file is the normal picture of the character. It must be a PNG with a transparent background, with the character facing right, 40–1000 px wide and 100–1000 px high (about 400 px high is best), at most 8 MB."),
    el("div", {}, pick)];
  if (!me.admin) body.push(el("p", {class: "hint md-typescale-body-small"}, MOD_NOTE));
  return body;
}

function addBackgroundBody() {
  const add = async () => {
    const value = name.value.trim().toLowerCase();
    if (!nameOK(value)) return;
    const f = await pickFile("image/png,image/jpeg");
    if (f && await act(() => api("POST", "/api/backgrounds", form(f, {name: value})), "Added.", pick)) { keepMessage = true; go("backgrounds", value); }
  };
  const name = nameField("Name", add);
  const pick = el("md-filled-button", {onclick: add}, "Select a PNG or JPEG file");
  const body = [name,
    el("p", {class: "hint md-typescale-body-small"}, "The file must be 160–2000 px each side, at most 8 MB. Square, 320–640 px, is best."),
    el("div", {}, pick)];
  if (!me.admin) body.push(el("p", {class: "hint md-typescale-body-small"}, MOD_NOTE));
  return body;
}

function backgroundBody(b) {
  const body = [
    el("img", {class: "picture", src: "/img/background/" + enc(b.name), alt: "Background " + b.name}),
    el("div", {class: "group"}, visibleSwitch("background", b),
      el("p", {class: "hint md-typescale-body-small"}, "A hidden background is not picked at random and is not in the comic editor.")),
  ];
  if (!b.custom) {
    body.push(el("p", {class: "hint md-typescale-body-medium"}, "A built-in background. You can hide it, but you cannot change it."));
    return body;
  }
  const url = "/api/backgrounds/" + enc(b.name);
  const undoKey = "background/" + b.name;
  const actions = el("div", {class: "panel-actions"});
  if (me.admin) {
    actions.append(el("md-outlined-button", {"data-key": "replace:" + b.name, ariaLabel: "Replace " + b.name, onclick: () => {
      confirmDanger("Replace " + b.name + "?", "New comics will use the new file. An operator can restore the old file from the trash folder.", "Select the new file", async () => {
        const f = await pickFile("image/png,image/jpeg");
        return f ? act(() => api("POST", "/api/backgrounds", form(f, {name: b.name, replace: "true"})), "Replaced.") : false;
      });
    }}, "Replace"));
    actions.append(el("md-outlined-button", {"data-key": "remove:" + b.name, ariaLabel: "Remove " + b.name, onclick: () => {
      confirmDanger("Remove " + b.name + "?", "New comics cannot use this background. " + TRASH, "Remove",
        () => act(() => api("DELETE", url), "Removed."));
    }}, "Remove"));
  } else if (art.undo[undoKey]) {
    actions.append(el("md-outlined-button", {"data-key": "remove:" + b.name, ariaLabel: "Undo the upload of " + b.name, onclick: () => {
      confirmDanger("Undo the upload of " + b.name + "?", undoLeft(undoKey) + " New comics cannot use this background. " + TRASH, "Undo upload",
        () => act(() => api("DELETE", url), "Upload undone."));
    }}, "Undo upload"));
  } else {
    actions.append(el("p", {class: "hint md-typescale-body-small"}, "Only an Administrator can replace or remove this background."));
  }
  body.push(actions);
  return body;
}

function poseCard(c, pose) {
  const has = pose === "icon" ? c.iconFile : c.poses.includes(pose);
  const url = "/api/characters/" + enc(c.name) + "/poses/" + pose;
  const key = "pose:" + c.name + ":" + pose;
  const undoKey = "pose/" + c.name + "/" + pose;
  const card = el("div", {class: "pose"}, el("span", {class: "md-typescale-label-medium"}, pose));
  if (pose === "icon" || has) card.append(el("img", {src: (pose === "icon" ? "/img/icon/" + enc(c.name) : "/img/pose/" + enc(c.name) + "/" + pose), alt: pose + " pose of " + c.name}));
  else card.append(el("span", {class: "missing hint md-typescale-body-small"}, "not set"));
  if (!has && pose !== "neutral") {
    const b = el("md-text-button", {"data-key": key, ariaLabel: "Add " + pose + " pose", onclick: async () => {
      const f = await pickFile("image/png");
      if (f) { await act(() => api("POST", url, form(f, {replace: "false"})), "Saved.", b); focusKey(key); }
    }}, "Add");
    card.append(b);
  } else if (me.admin) {
    card.append(el("md-text-button", {"data-key": key, ariaLabel: "Replace " + pose + " pose", onclick: () => {
      confirmDanger("Replace the " + pose + " pose of " + c.name + "?", "New comics will use the new file. An operator can restore the old file from the trash folder.", "Select the new file", async () => {
        const f = await pickFile("image/png");
        return f ? act(() => api("POST", url, form(f, {replace: "true"})), "Saved.") : false;
      });
    }}, "Replace"));
  }
  if (has && pose !== "neutral" && pose !== "icon" && (me.admin || art.undo[undoKey])) {
    const verb = me.admin ? "Remove" : "Undo upload";
    card.append(el("md-text-button", {"data-key": key + ":remove", ariaLabel: verb + ": " + pose + " pose", onclick: () => {
      confirmDanger(verb + ": " + pose + " pose of " + c.name + "?",
        (me.admin ? "" : undoLeft(undoKey) + " ") + TRASH, verb,
        () => act(() => api("DELETE", url), me.admin ? "Removed." : "Upload undone."));
    }}, verb));
  }
  return card;
}

function characterBody(c) {
  const used = art.used[c.name] || 0;
  const status = el("div", {class: "group"}, visibleSwitch("character", c),
    el("p", {class: "hint md-typescale-body-small"}, "A hidden character cannot be chosen and is not given at random. Members who already have it keep it."),
    el("p", {class: "md-typescale-body-medium"}, plural(used, "member has this character.", "members have this character.")),
    el("div", {}, el("md-text-button", {href: "#/comics/preview/" + enc(c.name), style: "margin-left:-12px"}, "Draw a test comic")));
  if (!c.custom) {
    return [
      el("img", {class: "picture", src: "/img/portrait/" + enc(c.name), alt: c.name}),
      status,
      el("p", {class: "hint md-typescale-body-medium"}, "A built-in Comic Chat character. You can hide it, but you cannot change it."),
    ];
  }
  const setFace = (x, y, button) => act(() => api("PATCH", "/api/characters/" + enc(c.name), {face: [x, y]}), "The face point is saved.", button);
  const dot = el("div", {class: "face-dot", style: "left:" + (100 * c.face[0] / c.width) + "%;top:" + (100 * c.face[1] / c.height) + "%"});
  const img = el("img", {src: "/img/pose/" + enc(c.name) + "/neutral", alt: "Neutral pose. The face point is at " + c.face[0] + ", " + c.face[1] + "."});
  const box = el("div", {class: "face-box", onclick: e => {
    const r = img.getBoundingClientRect();
    setFace(Math.floor((e.clientX - r.left) / r.width * img.naturalWidth), Math.floor((e.clientY - r.top) / r.height * img.naturalHeight));
  }}, img, dot);
  // The same point with the keyboard.
  const fx = el("md-outlined-text-field", {label: "Face X", type: "number", value: String(c.face[0]), min: "0", max: String(c.width - 1)});
  const fy = el("md-outlined-text-field", {label: "Face Y", type: "number", value: String(c.face[1]), min: "0", max: String(c.height - 1)});
  const faceSave = el("md-filled-tonal-button", {"data-key": "face:" + c.name, onclick: async () => {
    if (!/^\d+$/.test(fx.value) || !/^\d+$/.test(fy.value)) { toast("Type whole numbers for Face X and Face Y.", true); return; }
    await setFace(Number(fx.value), Number(fy.value), faceSave);
    focusKey("face:" + c.name);
  }}, "Save");

  const saved = (c.users || []).join("\n");
  const users = el("md-outlined-text-field", {label: "Only these users can use it", type: "textarea", rows: 2, class: "wide",
    value: usersDraft[c.name] !== undefined ? usersDraft[c.name] : saved,
    oninput: () => { usersDraft[c.name] = users.value; }});
  const usersSave = el("md-filled-tonal-button", {"data-key": "users:" + c.name, onclick: async () => {
    const list = users.value.split(/[\s,]+/).filter(Boolean);
    const save = async () => {
      delete usersDraft[c.name];
      const ok = await act(() => api("PATCH", "/api/characters/" + enc(c.name), {users: list}), "Saved.", usersSave);
      if (!ok) usersDraft[c.name] = users.value;
      focusKey("users:" + c.name);
      return ok;
    };
    if (!list.length && saved) {
      confirmDanger("Make " + c.name + " public?", "With no IDs, all members can choose this character, and comics can give it to anyone.", "Make it public", save);
      return;
    }
    await save();
  }}, "Save the list");

  const body = [
    el("div", {class: "group"}, el("h3", {class: "md-typescale-label-large"}, "Face point"),
      el("p", {class: "hint md-typescale-body-small"}, "Click the middle of the face, or type the point. Balloon tails point there."),
      box, el("div", {class: "fields"}, fx, fy, faceSave)),
    status,
    el("div", {class: "group"}, el("h3", {class: "md-typescale-label-large"}, "Private"), users,
      el("p", {class: "hint md-typescale-body-small"}, "Paste Discord user IDs, one on each line. With no IDs, all members can use this character."),
      el("div", {}, usersSave)),
    el("div", {class: "group"}, el("h3", {class: "md-typescale-label-large"}, "Poses"),
      el("p", {class: "hint md-typescale-body-small"}, "The bot selects a pose from the words in a message. Only the neutral pose is necessary."),
      el("div", {class: "poses"}, ...me.poses.map(p => poseCard(c, p)))),
  ];
  const undoKey = "character/" + c.name;
  const actions = el("div", {class: "panel-actions"});
  if (me.admin || art.undo[undoKey]) {
    const verb = me.admin ? "Remove" : "Undo upload";
    actions.append(el("md-outlined-button", {"data-key": "remove:" + c.name, onclick: () => {
      confirmDanger(verb + ": " + c.name + "?",
        (me.admin ? "" : undoLeft(undoKey) + " ") +
        (used ? (used === 1 ? "1 member uses this character. Comics will give that member another character. " : used + " members use this character. Comics will give them other characters. ") : "") + TRASH,
        verb, () => act(() => api("DELETE", "/api/characters/" + enc(c.name)), me.admin ? "Removed." : "Upload undone."));
    }}, verb + " this character"));
  } else {
    actions.append(el("p", {class: "hint md-typescale-body-small"}, "Only an Administrator can replace or remove this character's files."));
  }
  body.push(actions);
  return body;
}

function memberBody(m) {
  const sel = el("md-outlined-select", {label: "Character", class: "wide"});
  setOptions(sel, art.characters.map(c => [c.name, c.name + (c.users && c.users.length ? " (private)" : "") + (c.hidden ? " (hidden)" : "")]), true, m.character || "");
  const lock = el("md-switch", {selected: !!m.locked, ariaLabel: "Lock the character. The member cannot change it."});
  const refresh = async () => {
    await loadMembers();
    $("member-found").replaceChildren();
    renderPanel();
  };
  const save = el("md-filled-button", {onclick: async () => {
    if (!sel.value) { toast("Select a character.", true); return; }
    const character = sel.value, locked = lock.selected;
    const ok = await act(() => api("PUT", "/api/members/" + m.id, {character, locked}), (m.name || m.id) + " has the character " + character + (locked ? ", locked. They cannot change it with /character." : "."), save);
    if (ok) { known[m.id] = {...m, character, locked}; await refresh(); }
  }}, "Save");
  const actions = el("div", {class: "panel-actions"}, save);
  if (m.character) actions.append(el("md-outlined-button", {onclick: () => {
    confirmDanger("Clear the character of " + (m.name || m.id) + "?",
      m.locked ? "This also removes the lock. Comics will give the member a character until they choose one." : "Comics will give the member a character until they choose one.", "Clear",
      async () => {
        const ok = await act(() => api("DELETE", "/api/members/" + m.id), "Cleared.");
        if (ok) { known[m.id] = {...m, character: "", locked: false}; await refresh(); }
        return ok;
      });
  }}, "Clear"));
  return [
    el("div", {class: "member"}, m.avatar ? el("img", {src: m.avatar, alt: ""}) : el("div", {class: "noavatar"}),
      el("div", {class: "who id label md-typescale-body-small"}, m.id)),
    sel,
    el("label", {class: "row md-typescale-body-medium"}, "Lock (the member cannot change it)", lock),
    actions,
  ];
}

// ---- Speech. ----

let speech = null;
let speechLoading = null;

function loadSpeech(fresh) {
  if (speech && !fresh) { return Promise.resolve(); }
  if (speechLoading) return speechLoading;
  speechLoading = api("GET", "/api/speech" + (fresh ? "?fresh=1" : "")).then(data => {
    speech = data;
    if (here.area === "speech") { renderView(); renderPanel(); }
  }).catch(e => toast(e.message, true)).finally(() => { speechLoading = null; });
  return speechLoading;
}

const channelOptions = list => list.map(c => [c.id, "#" + c.name]);

function noChannelsHint(setting) {
  return "You have no channel to post in. An Administrator can check " + setting + " in Settings.";
}

let sendFilled = null;
function renderSend() {
  if (!speech) return;
  if (sendFilled !== speech) {
    sendFilled = speech;
    $("send-text").maxLength = speech.maxChars;
    $("send-text").supportingText = "At most " + speech.maxChars + " characters.";
    setOptions($("send-voice"), [["random", "At random"], ...speech.voices.map(v => [v, v])]);
    setOptions($("send-channel"), channelOptions(speech.channels), true);
    $("send-go").disabled = speech.channels.length === 0;
    if (!speech.channels.length) toast(noChannelsHint("TTS_CHANNELS"), true);
  }
  loadJobs();
}

let jobsTimer = null;
async function loadJobs() {
  clearTimeout(jobsTimer);
  const jobs = await api("GET", "/api/speech/jobs").catch(() => null);
  if (!jobs || here.view !== "send") return;
  const name = id => { const c = speech && speech.channels.find(x => x.id === id); return c ? "#" + c.name : id; };
  $("send-jobs").replaceChildren(...(jobs.length ? jobs.map(j => {
    const tag = el("span", {class: "tag " + (j.state === "failed" ? "limit" : j.state === "done" ? "use" : "off")},
      j.state === "done" ? "posted" : j.state === "failed" ? "failed" : "working");
    const row = el("div", {class: "rowitem"},
      el("div", {class: "rowhead md-typescale-label-large"}, tag, el("span", {}, j.voice), el("span", {class: "hint"}, name(j.channel))),
      el("div", {class: "md-typescale-body-medium pre"}, j.text));
    if (j.state === "done" && j.link) row.append(el("a", {href: j.link, target: "_blank", rel: "noopener", class: "md-typescale-body-small"}, "Open it in Discord"));
    else row.append(el("div", {class: "hint md-typescale-body-small"}, j.detail));
    return row;
  }) : [el("p", {class: "hint md-typescale-body-medium"}, "You have made no clips here since the bot started.")]));
  if (jobs.some(j => j.state === "working")) jobsTimer = setTimeout(loadJobs, 2000);
}

async function sendClip() {
  const text = $("send-text").value.trim();
  if (!text) { toast("Type the text.", true); return; }
  if (!$("send-channel").value) { toast("Select a channel.", true); return; }
  const ok = await act(() => api("POST", "/api/speech/send", {text, voice: $("send-voice").value, channel: $("send-channel").value, voiceMsg: $("send-voicemsg").selected}),
    "The clip is in the queue.", $("send-go"));
  if (ok) { $("send-text").value = ""; loadJobs(); }
}

let clipData = {day: "", clips: [], days: null, saving: true};
async function loadClipDays() {
  const data = await api("GET", "/api/speech/days").catch(e => { toast(e.message, true); return null; });
  if (!data) return;
  clipData.days = data.days; clipData.saving = data.saving;
  setOptions($("clip-day"), data.days.map(d => [d.day, d.day + " (" + d.count + ")"]), true, clipData.day || (data.days[0] && data.days[0].day) || "");
  if (data.days.length) {
    await loadClips(clipData.day || data.days[0].day);
  }
  if (here.view === "clips") { renderClips(); renderPanel(); }
}

async function loadClips(day) {
  const data = await api("GET", "/api/speech/clips?day=" + enc(day)).catch(e => { toast(e.message, true); return null; });
  clipData.day = day; clipData.clips = data ? data.clips : [];
  const voices = [...new Set(clipData.clips.map(c => c.voice))].sort();
  setOptions($("clip-voice"), [["*", "All voices"], ...voices.map(v => [v, v])]);
}

function clipURL(day, clip) { return "/audio/clip/" + enc(day) + "/" + enc(clip.file); }
const clipTime = file => /^\d{6}/.test(file) ? file.slice(0, 2) + ":" + file.slice(2, 4) + ":" + file.slice(4, 6) : "";
const clipLength = c => c.audio_seconds ? Math.round(c.audio_seconds) + " s" : "";

function renderClips() {
  const host = $("clip-list");
  if (!clipData.days) { host.replaceChildren(el("p", {class: "hint md-typescale-body-medium"}, "Loading the clips…")); return; }
  if (!clipData.saving) { host.replaceChildren(el("p", {class: "hint md-typescale-body-medium"}, "The speech worker does not save clips. An operator must set CLIPS_DIR.")); return; }
  if (!clipData.days.length) { host.replaceChildren(el("p", {class: "hint md-typescale-body-medium"}, "There are no saved clips yet.")); return; }
  const q = $("clip-filter").value.trim().toLowerCase(), v = $("clip-voice").value;
  const list = clipData.clips.filter(c => (v === "*" || !v || c.voice === v) && c.text.toLowerCase().includes(q));
  host.replaceChildren(...(list.length ? list.map(c => {
    const id = clipData.day + "/" + c.file;
    const post = el("md-outlined-button", {href: "#/speech/clips/" + enc(id), "data-key": "open:clips:" + id, ariaLabel: "Post the clip: " + c.text.slice(0, 60)}, "Post");
    return el("div", {class: "rowitem"},
      el("div", {class: "rowhead"},
        el("span", {class: "grow md-typescale-label-large"}, c.voice),
        el("span", {class: "hint md-typescale-body-small id"}, [clipTime(c.file), clipLength(c)].filter(Boolean).join(" · ")), post),
      el("div", {class: "md-typescale-body-medium pre"}, c.text || "(no text saved)"),
      el("audio", {controls: true, preload: "none", src: clipURL(clipData.day, c), "aria-label": "Play this clip"}));
  }) : [el("p", {class: "hint md-typescale-body-medium"}, "No clip matches this search.")]));
}

function clipBody(day, c) {
  const channel = el("md-outlined-select", {label: "Channel", class: "wide"});
  setOptions(channel, channelOptions(speech.channels), true);
  const voiceMsg = el("md-switch", {selected: true, ariaLabel: "Post as a voice message"});
  const go = el("md-filled-button", {disabled: speech.channels.length === 0, onclick: async () => {
    if (!channel.value) { toast("Select a channel.", true); return; }
    const ok = await act(() => api("POST", "/api/speech/clips/post", {day, file: c.file, channel: channel.value, voiceMsg: voiceMsg.selected}), "Posted.", go);
    if (ok && ok.link) $("panel-body").append(el("a", {href: ok.link, target: "_blank", rel: "noopener"}, "Open it in Discord"));
  }}, "Post this clip");
  const body = [
    el("p", {class: "md-typescale-body-medium pre"}, c.text || "(no text saved)"),
    el("p", {class: "hint md-typescale-body-small"}, [day, clipTime(c.file), clipLength(c)].filter(Boolean).join(" · ")),
    el("audio", {controls: true, preload: "metadata", src: clipURL(day, c), "aria-label": "Play this clip"}),
    channel,
    el("label", {class: "row md-typescale-body-medium"}, "Post as a voice message (no text with it)", voiceMsg),
    el("div", {}, go),
  ];
  if (!speech.channels.length) body.push(el("p", {class: "hint md-typescale-body-small"}, noChannelsHint("TTS_CHANNELS")));
  return body;
}

function renderVoices() {
  if (!speech) return;
  $("voice-tools").replaceChildren(...(me.admin
    ? [el("md-filled-button", {href: "#/speech/voices/+"}, "Add voice")]
    : [el("p", {class: "hint md-typescale-body-small"}, "Only an Administrator can add, replace or remove voices.")]));
  keepFocus(() => $("voice-list").replaceChildren(...(speech.voices.length ? speech.voices.map(v => {
    const row = el("div", {class: "rowitem"}, el("div", {class: "rowhead"}, el("span", {class: "grow md-typescale-body-large"}, v)));
    if (me.admin) {
      const head = row.firstChild;
      head.append(el("md-text-button", {"data-key": "replace:voice:" + v, ariaLabel: "Replace the voice " + v, onclick: () => {
        confirmDanger("Replace the voice " + v + "?", "New clips will use the new file. " + TRASH, "Select the new file", async () => {
          const f = await pickFile(VOICE_ACCEPT);
          return f ? uploadVoice(v, f, true) : false;
        });
      }}, "Replace"));
      head.append(el("md-text-button", {"data-key": "remove:voice:" + v, ariaLabel: "Remove the voice " + v, onclick: () => {
        confirmDanger("Remove the voice " + v + "?", "New clips cannot use this voice. Saved clips stay. " + TRASH, "Remove", async () => {
          const data = await act(() => api("DELETE", "/api/speech/voices/" + enc(v)), "Removed.");
          if (data) { speech.voices = data.voices; sendFilled = null; renderVoices(); }
          return data;
        });
      }}, "Remove"));
    }
    return row;
  }) : [el("p", {class: "hint md-typescale-body-medium"}, "There are no voices yet.")])));
}

const VOICE_ACCEPT = ".wav,.mp3,.flac,.ogg,.m4a,audio/*";

async function uploadVoice(name, file, replace) {
  const data = await act(() => api("POST", "/api/speech/voices", form(file, {name, replace: replace ? "true" : "false"})), "Saved.");
  if (data) { speech.voices = data.voices; sendFilled = null; if (here.view === "voices") renderVoices(); }
  return data;
}

function addVoiceBody() {
  const add = async () => {
    const value = name.value.trim().toLowerCase();
    if (!nameOK(value)) return;
    if (value === "random") { toast("The name \"random\" is not available.", true); return; }
    const f = await pickFile(VOICE_ACCEPT);
    if (!f) return;
    pick.disabled = true;
    const ok = await uploadVoice(value, f, false);
    pick.disabled = false;
    if (ok) { keepMessage = true; go("voices"); }
  };
  const name = nameField("Name", add);
  const pick = el("md-filled-button", {onclick: add}, "Select an audio file");
  return [name,
    el("p", {class: "hint md-typescale-body-small"}, "A WAV, MP3, FLAC, OGG or M4A file, at most 20 MB, with one clear voice. The worker uses at most the first 60 seconds. The name shows in the voice list."),
    el("div", {}, pick)];
}

// ---- Markov and Persona. ----

const CHATS = {
  markov: {chain: "markov", setting: "MARKOV_REPLY_CHANNELS", intro: "Type a message. The chain makes replies, and you can send one to an approved channel."},
  persona: {chain: "persona", setting: "PERSONA_REPLY_CHANNELS", intro: "Type a message to the persona. The chain makes replies, and you can send one to an approved channel."},
};
const chatState = {};

function renderChat(view) {
  const cfg = CHATS[view];
  let st = chatState[view];
  if (!st) {
    st = chatState[view] = {result: null, sent: {}};
    st.input = el("md-outlined-text-field", {label: "Message", type: "textarea", rows: 2, maxLength: 2000, class: "wide"});
    st.go = el("md-filled-button", {onclick: () => generate(view)}, "Generate");
    st.channel = el("md-outlined-select", {label: "Send to"});
    st.out = el("div", {});
    $(view).replaceChildren(el("div", {class: "narrow-form"},
      el("p", {class: "hint md-typescale-body-medium"}, cfg.intro), st.input, el("div", {}, st.go)), st.out);
  }
}

async function generate(view) {
  const st = chatState[view];
  const text = st.input.value.trim();
  if (!text) { toast("Type a message.", true); return; }
  const data = await act(() => api("POST", "/api/chat/" + CHATS[view].chain + "/generate", {text}), "", st.go);
  if (!data || data === true) return;
  st.result = data; st.sent = {};
  drawCandidates(view);
}

function drawCandidates(view) {
  const st = chatState[view], d = st.result, cfg = CHATS[view];
  const kids = [];
  setOptions(st.channel, channelOptions(d.channels), true);
  kids.push(el("div", {class: "toolbar", style: "margin:16px 0 8px"}, st.channel));
  if (!d.channels.length) kids.push(el("p", {class: "hint md-typescale-body-small"}, noChannelsHint(cfg.setting)));
  if (d.question) kids.push(el("p", {class: "hint md-typescale-body-small"}, "The ranker got this question: " + d.question));
  if (d.note) kids.push(el("p", {class: "hint md-typescale-body-small"}, d.note));
  const rows = el("div", {class: "rows"});
  for (const c of d.candidates) {
    const send = el("md-outlined-button", {disabled: !d.channels.length, ariaLabel: "Send this reply: " + c.text.slice(0, 60), onclick: async () => {
      if (!st.channel.value) { toast("Select a channel in Send to.", true); return; }
      const ok = await act(() => api("POST", "/api/chat/" + cfg.chain + "/send", {id: c.id, channel: st.channel.value}), "Sent.", send);
      if (ok) {
        const link = ok.link ? el("a", {href: ok.link, target: "_blank", rel: "noopener", class: "md-typescale-label-large"}, "Open it in Discord") : el("span", {class: "tag use"}, "sent");
        send.replaceWith(link);
      }
    }}, "Send");
    rows.append(el("div", {class: "rowitem"}, el("div", {class: "rowhead"},
      c.score !== undefined ? el("span", {class: "tag id", title: "Higher is better"}, c.score.toFixed(2)) : "",
      el("span", {class: "grow md-typescale-body-medium pre"}, c.text), send)));
  }
  kids.push(rows);
  st.out.replaceChildren(...kids);
}

// ---- Tickets. ----

let tickets = {list: null, more: false, searched: false};
const ticketDetail = {};
const ticketStatusText = {open: "Open", closed: "Closed", archived: "Archived"};

async function loadTickets() {
  const user = $("ticket-user").value.trim(), q = $("ticket-q").value.trim();
  const data = await api("GET", "/api/tickets?user=" + enc(user) + "&q=" + enc(q)).catch(e => { toast(e.message, true); return null; });
  if (!data) return;
  tickets = {list: data.tickets, more: data.more, searched: !!(user || q)};
  // An open ticket gets new messages: read each transcript again.
  for (const k of Object.keys(ticketDetail)) delete ticketDetail[k];
  if (here.view === "tickets") renderTickets();
}

function ticketWho(t) {
  if (t.kind === "mod") return "Mod ticket" + (t.members ? " with " + t.members : "");
  return "Opened by " + t.username + " (" + t.userId + ")";
}

function renderTickets() {
  const host = $("ticket-list");
  const hint = text => el("p", {class: "hint md-typescale-body-medium"}, text);
  if (!tickets.list) { host.replaceChildren(hint("Loading the tickets…")); return; }
  if (!tickets.list.length) { host.replaceChildren(hint(tickets.searched ? "No ticket matches this search." : "There are no tickets yet.")); return; }
  const rows = tickets.list.map(t => {
    const props = {class: "rowitem", href: "#/tickets/tickets/" + enc(t.key), "data-key": "open:tickets:" + t.key};
    if (here.item === t.key) props["aria-current"] = "true";
    return el("a", props,
      el("div", {class: "rowhead"},
        el("span", {class: "grow md-typescale-label-large mono"}, t.ref),
        el("span", {class: "hint md-typescale-body-small"}, ticketStatusText[t.status] || t.status)),
      el("div", {class: "md-typescale-body-medium"}, ticketWho(t)),
      el("div", {class: "hint md-typescale-body-small id"}, ["Opened " + (t.opened || "at an unknown time"), plural(t.messages, "message", "messages")].join(" · ")));
  });
  if (tickets.more) rows.push(hint("Only the newest 100 tickets show. Search to find older ones."));
  host.replaceChildren(...rows);
}

function ticketFile(f) {
  const url = "/files/ticket/" + f.id;
  if (!f.saved) return el("p", {class: "hint md-typescale-body-small"}, "📎 " + f.name + " (not saved: " + f.note + ")");
  if (f.media === "img") return el("a", {href: url, target: "_blank", rel: "noopener"}, el("img", {src: url, alt: f.name, loading: "lazy"}));
  if (f.media === "video") return el("video", {src: url, controls: true, preload: "metadata", "aria-label": f.name});
  if (f.media === "audio") return el("audio", {src: url, controls: true, preload: "none", "aria-label": f.name});
  return el("a", {href: url, download: f.name}, "📎 " + f.name + " (" + Math.max(1, Math.round(f.size / 1024)) + " KB)");
}

function ticketBody(d) {
  const body = d.facts.map(f => el("p", {class: "hint md-typescale-body-small"}, f));
  if (d.link) body.push(el("p", {}, el("a", {href: d.link, target: "_blank", rel: "noopener"}, "Open the channel in Discord")));
  if (d.notes.length) {
    body.push(el("h3", {class: "md-typescale-title-small"}, "Notes"));
    for (const n of d.notes) body.push(el("div", {class: "tmsg"},
      el("div", {class: "md-typescale-body-medium pre"}, n.note),
      el("div", {class: "hint md-typescale-body-small"}, (n.author || "unknown") + " · " + n.at)));
  }
  body.push(el("h3", {class: "md-typescale-title-small"}, "Messages"));
  if (d.legacy !== undefined) {
    body.push(el("div", {class: "md-typescale-body-medium pre mono"}, d.legacy || "(empty)"));
    return body;
  }
  if (!d.messages.length) body.push(el("p", {class: "hint md-typescale-body-medium"}, "The bot has no messages for this ticket."));
  for (const m of d.messages) {
    const when = [m.sent + " UTC", m.edited ? "edited " + m.edited : ""].filter(Boolean).join(" · ");
    const kids = [el("div", {class: "rowhead"},
      el("span", {class: "who md-typescale-label-large", title: m.authorId}, m.author),
      el("span", {class: "hint md-typescale-body-small id"}, when))];
    if (m.deleted) kids.push(el("div", {class: "gone md-typescale-body-small"}, "Deleted " + m.deleted + " UTC"));
    for (const e of m.edits || []) kids.push(el("div", {class: "old md-typescale-body-small"}, "Before the edit: " + e.content));
    if (m.content) kids.push(el("div", {class: "md-typescale-body-medium pre"}, m.content));
    for (const f of m.files || []) kids.push(ticketFile(f));
    body.push(el("div", {class: "tmsg" + (m.bot ? " bot" : "")}, ...kids));
  }
  return body;
}

// ---- Settings. ----

let settings = null, guildLists = null;

async function loadSettings() {
  const data = await Promise.all([api("GET", "/api/settings"), api("GET", "/api/guild")]).catch(e => { toast(e.message, true); return null; });
  if (!data) return;
  settings = data[0]; guildLists = data[1];
  if (here.view === "settings") { renderSettings(); renderPanel(); }
}

function nameOf(list, id, prefix) {
  const n = list.find(x => x.id === id);
  return n ? prefix + n.name : id + " (not found)";
}

// settingText is a value as words: names, not IDs.
function settingText(k) {
  const v = k.value;
  const g = guildLists || {channels: [], roles: []};
  switch (k.kind) {
    case "bool": return v === "on" || v === "true" ? "On" : "Off";
    case "choice": { const c = (k.choices || []).find(x => x.Value === v); return c ? c.Label : v; }
    case "channel": case "category": return v ? nameOf(g.channels, v, k.kind === "channel" ? "#" : "") : "Not set";
    case "role": return v ? nameOf(g.roles, v, "@") : "Not set";
    case "user": return v ? (k.display ? k.display + " (" + v + ")" : v) : "Not set";
    case "channel_list": return v ? v.split(",").map(id => nameOf(g.channels, id, "#")).join(", ") : "None";
    case "role_list": return v ? v.split(",").map(id => nameOf(g.roles, id, "@")).join(", ") : "None";
  }
  return v === "" ? "Not set" : v;
}

function renderSettings() {
  $("sessions-end").hidden = !me.admin;
  $("settings-note").textContent = me.admin ? "Click a setting to change it. Each change goes in the mod log." : "You can read the settings. Only an Administrator can change them.";
  if (!settings) { $("settings-list").replaceChildren(el("p", {class: "hint md-typescale-body-medium"}, "Loading the settings…")); return; }
  const q = $("settings-filter").value.trim().toLowerCase();
  const out = [];
  for (const sec of settings.sections) {
    const hits = sec.settings.filter(k => !q || [k.key, k.description, settingText(k), sec.title].some(t => t.toLowerCase().includes(q)));
    if (!hits.length) continue;
    out.push(el("section", {class: "settings-section"}, el("h2", {class: "md-typescale-title-medium"}, sec.title),
      el("div", {class: "rows"}, ...hits.map(k => {
        const a = el("a", {class: "rowitem", href: "#/settings/settings/" + enc(k.key), "data-key": "open:settings:" + k.key},
          el("div", {class: "rowhead"}, el("span", {class: "grow mono"}, k.key), el("span", {class: "md-typescale-label-large"}, settingText(k))),
          el("div", {class: "hint md-typescale-body-small"}, k.description));
        if (here.item === k.key) a.setAttribute("aria-current", "true");
        return a;
      }))));
  }
  keepFocus(() => $("settings-list").replaceChildren(...(out.length ? out : [el("p", {class: "hint md-typescale-body-medium"}, "No setting matches this search.")])));
}

// settingEditor makes the control for a kind of setting.
function settingEditor(k) {
  const g = guildLists || {channels: [], roles: []};
  const select = (options, label) => {
    const sel = el("md-outlined-select", {label, class: "wide"});
    setOptions(sel, options, false, k.value === "" ? "*" : k.value);
    return {node: sel, get: () => sel.value === "*" ? "" : sel.value};
  };
  const checks = (list, prefix) => {
    const chosen = new Set(k.value ? k.value.split(",") : []);
    const boxes = list.map(x => {
      const box = el("md-checkbox", {checked: chosen.has(x.id), ariaLabel: x.name});
      box.dataset.id = x.id;
      return box;
    });
    return {node: el("div", {class: "checks"}, ...list.map((x, i) => el("label", {class: "md-typescale-body-medium"}, boxes[i], prefix + x.name))),
      get: () => boxes.filter(b => b.checked).map(b => b.dataset.id).join(",")};
  };
  switch (k.kind) {
    case "bool": {
      const s = el("md-switch", {selected: k.value === "on" || k.value === "true", ariaLabel: k.key});
      return {node: el("label", {class: "row md-typescale-body-medium"}, "On", s), get: () => s.selected ? "on" : "off"};
    }
    case "choice": return select((k.choices || []).map(c => [c.Value, c.Label]), "Value");
    case "channel": return select([["*", "Not set"], ...g.channels.filter(c => c.kind === "channel").map(c => [c.id, "#" + c.name])], "Channel");
    case "category": return select([["*", "Not set"], ...g.channels.filter(c => c.kind === "category").map(c => [c.id, c.name])], "Category");
    case "role": return select([["*", "Not set"], ...g.roles.map(r => [r.id, "@" + r.name])], "Role");
    case "channel_list": return checks(g.channels.filter(c => c.kind === "channel"), "#");
    case "role_list": return checks(g.roles, "@");
  }
  const props = {label: k.kind === "user" ? "User ID" : "Value", value: k.value, class: "wide"};
  if (k.kind === "long_text") { props.type = "textarea"; props.rows = 4; props.maxLength = 500; }
  if (k.kind === "int") props.type = "number";
  const field = el("md-outlined-text-field", props);
  return {node: field, get: () => field.value};
}

function settingBody(k) {
  const body = [
    el("p", {class: "md-typescale-body-medium"}, k.description),
    el("div", {class: "group"}, el("h3", {class: "md-typescale-label-large"}, "Now"), el("p", {class: "md-typescale-body-medium pre"}, settingText(k))),
  ];
  if (k.default !== "" && k.kind !== "channel_list" && k.kind !== "role_list") {
    body.push(el("p", {class: "hint md-typescale-body-small"}, "Default: " + k.default));
  }
  if (!me.admin) {
    body.push(el("p", {class: "hint md-typescale-body-small"}, "Only an Administrator can change this setting."));
    return body;
  }
  const editor = settingEditor(k);
  const save = el("md-filled-button", {onclick: async () => {
    const value = editor.get();
    const ok = await act(() => api("PUT", "/api/settings/" + enc(k.key), {value}), "Saved.", save);
    if (!ok) return;
    k.value = ok.value;
    // A setting can turn an area on or off, or change who can post where.
    me = {...me, ...await api("GET", "/api/me")};
    speech = null;
    renderSettings();
    renderPanel();
  }}, "Save");
  body.push(editor.node);
  if (k.key === "MOD_SUPPORT") body.push(el("p", {class: "hint md-typescale-body-small"}, "After a change, post the ticket message again with /setup."));
  body.push(el("div", {}, save));
  return body;
}

// ---- The events. ----

// nameOK checks a new name before the file pick, with the rule of the server.
function nameOK(name) {
  if (!name) { toast("Type a name, then select the file.", true); return false; }
  if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(name)) {
    toast("The name must have only a–z, 0–9, - and _, and start with a letter or number.", true);
    return false;
  }
  return true;
}

// On a narrow screen, the menu and the panel cover the page. What is behind
// them must not get the focus.
const narrow = window.matchMedia("(max-width: 899px)");
function syncCovered() {
  const menu = $("menu").classList.contains("open");
  $("work").inert = menu;
  $("area").inert = narrow.matches && !$("panel").hidden;
  $("topbar").inert = narrow.matches && !$("panel").hidden;
}
narrow.addEventListener("change", syncCovered);

function openMenu() {
  $("menu").classList.add("open"); $("scrim").classList.add("open");
  syncCovered();
  requestAnimationFrame(() => $("nav-comics").focus());
}
function closeMenu() {
  const was = $("menu").classList.contains("open");
  $("menu").classList.remove("open"); $("scrim").classList.remove("open");
  syncCovered();
  return was;
}

window.addEventListener("hashchange", route);
$("menu-open").addEventListener("click", openMenu);
$("scrim").addEventListener("click", closeMenu);
$("panel-close").addEventListener("click", closePanel);
// This runs before the select handles the key: Esc closes an open list first,
// and not the panel behind it.
document.addEventListener("keydown", e => {
  if (e.key !== "Escape" || dialogOpen()) return;
  if ([...document.querySelectorAll("md-outlined-select")].some(x => x.open)) return;
  if (closeMenu()) { $("menu-open").focus(); return; }
  if (!$("panel").hidden) closePanel();
}, true);
$("send-go").addEventListener("click", sendClip);
$("clip-filter").addEventListener("input", renderClips);
$("clip-voice").addEventListener("change", renderClips);
$("clip-day").addEventListener("change", async () => {
  if ($("clip-day").value && $("clip-day").value !== clipData.day) { await loadClips($("clip-day").value); renderClips(); }
});
$("settings-filter").addEventListener("input", renderSettings);
// For a lost device, or an account that another person got into.
$("sessions-end").addEventListener("click", () => confirmDanger("End all logins?",
  "All mods must log in again, and you also. Use this when a device is lost, or when another person got into the account of a mod.",
  "End all logins", async () => {
    const ok = await act(() => api("POST", "/api/sessions/end"));
    if (ok) showLogin("All logins ended. Log in again.");
    return ok;
  }));
$("ticket-find").addEventListener("click", loadTickets);
for (const id of ["ticket-user", "ticket-q"]) $(id).addEventListener("keydown", e => { if (e.key === "Enter") loadTickets(); });
$("char-filter").addEventListener("input", renderCharacters);
$("back-filter").addEventListener("input", renderBackgrounds);
$("member-filter").addEventListener("input", renderMembers);
$("find-member").addEventListener("click", () => { focusSearch = true; });
$("member-find").addEventListener("click", findMember);
$("member-q").addEventListener("keydown", e => { if (e.key === "Enter") findMember(); });
$("prev-draw").addEventListener("click", drawPreview);
// A message in a closing confirmation must not be lost.
$("dialog").addEventListener("closed", () => {
  const m = $("dialog-msg"), text = m.textContent, kind = m.className.replace("msg", "").trim();
  m.textContent = ""; m.className = "msg";
  if (text) toast(text, kind);
});
$("logout").addEventListener("click", async () => {
  await api("POST", "/auth/logout").catch(() => {});
  showLogin("You are logged out.");
});

function bootError(text, retry) {
  $("loading").hidden = true;
  $("shell").hidden = true; $("login").hidden = true;
  $("boot-error").hidden = false;
  $("boot-error-text").textContent = text;
  $("boot-retry").hidden = !retry;
  $("boot-retry").onclick = retry;
}

async function start() {
  $("boot-error").hidden = true;
  $("loading").hidden = false;
  // A property set before its component loads is lost. The components are
  // a second file (material.js), so they can fail to load.
  const parts = Promise.all(["md-switch", "md-outlined-select", "md-dialog", "md-tabs", "md-primary-tab",
    "md-outlined-text-field", "md-linear-progress", "md-filter-chip", "md-chip-set", "md-icon-button", "md-checkbox"].map(n => customElements.whenDefined(n)));
  const late = new Promise(r => setTimeout(() => r("late"), 10000));
  if (await Promise.race([parts, late]) === "late") {
    bootError("The page components did not load. Check the connection, then reload the page.", () => location.reload());
    $("boot-retry").textContent = "Reload";
    return;
  }
  try {
    me = await api("GET", "/api/me");
  } catch (e) {
    if ($("login").hidden) bootError(e.message, start);
    return;
  }
  $("whoami").textContent = me.name + (me.admin ? " (admin)" : " (mod)");
  if (!me.off.comics) {
    try {
      art = await api("GET", "/api/art");
    } catch (e) {
      if ($("login").hidden) bootError(e.message, start);
      return;
    }
  }
  $("loading").hidden = true; $("login").hidden = true; $("shell").hidden = false;
  renderCounts();
  route();
}
start();
