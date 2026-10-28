// lasso's web view. A thin client of the daemon's API, like the TUI
// (internal/tui): it holds no state of its own beyond what's on screen.
// It signs in with the session cookie `lasso open` set, and listens on
// GET /events for changes. It never plays sounds, changes the page title
// or asks for notifications.
"use strict";

(function () {
  const TABS = ["issues", "actions", "doing", "summaries"];
  const HINTS = {
    issues: "open issues in your hot repos",
    actions: "workflow runs in your hot repos",
    doing: "what your bots are working on now",
    summaries: "the last 24 hours in each hot repo",
  };

  const state = {
    tab: "issues",
    items: { issues: [], actions: [], doing: [], summaries: [] },
    loaded: {},
    errs: {},
    sel: {},           // tab → selected id
    focus: null,       // the selected item's Focus, once loaded
    status: null,
    chat: [],
    chatErr: "",
    pending: "",
    changed: new Set(),
    connected: false,
    dropped: false,
    signedOut: false,
  };

  const $ = (id) => document.getElementById(id);

  function el(tag, attrs, ...kids) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v === true ? "" : v);
    }
    for (const kid of kids) {
      if (kid === null || kid === undefined || kid === false) continue;
      n.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return n;
  }

  // ago says how long before now t was, in the fewest words (as the TUI).
  function ago(t) {
    const d = (Date.now() - new Date(t).getTime()) / 1000;
    if (!isFinite(d)) return "";
    if (d < 60) return "just now";
    if (d < 3600) return Math.floor(d / 60) + "m ago";
    if (d < 48 * 3600) return Math.floor(d / 3600) + "h ago";
    return Math.floor(d / 86400) + "d ago";
  }

  function hhmm(t) {
    const d = new Date(t);
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }

  const zeroTime = (t) => !t || t.startsWith("0001-01-01");

  // --- API ---------------------------------------------------------------

  class SignedOut extends Error {}

  async function api(method, path, body) {
    const opts = { method, credentials: "same-origin", headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opts);
    if (resp.status === 401) {
      signedOut();
      throw new SignedOut("not signed in");
    }
    const text = await resp.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* plain text */ }
    if (!resp.ok) {
      const msg = (data && (data.detail || (data.decision && data.decision.reason))) || text.trim() || resp.statusText;
      const err = new Error(msg);
      err.status = resp.status;
      err.data = data;
      throw err;
    }
    return data;
  }

  function signedOut() {
    if (state.signedOut) return;
    state.signedOut = true;
    $("signin").hidden = false;
    $("panes").hidden = true;
    if (events) events.close();
  }

  // --- Header ------------------------------------------------------------

  function hotRepos() {
    const st = state.status;
    if (st && st.hot_repos && st.hot_repos.length) return st.hot_repos;
    // A daemon that doesn't report them: the repos the lists mention.
    const seen = new Set();
    for (const t of ["issues", "actions", "summaries"]) {
      for (const it of state.items[t]) if (it.repo) seen.add(it.repo);
    }
    return [...seen].sort();
  }

  function renderHeader() {
    const repos = hotRepos();
    $("repos").replaceChildren(...repos.map((r) => el("span", { text: r })));
    const parts = [];
    const st = state.status;
    if (st) {
      for (const b of st.bots || []) {
        const glyph = { working: "●", waiting: "◉", unreachable: "×" }[b.state] || "○";
        const words = b.kind + (b.count > 0 ? " " + b.count : "");
        parts.push(el("span", { title: b.kind + ": " + (b.state || "idle") },
          el("span", { class: b.state || "idle", "aria-hidden": "true", text: glyph }), " ",
          el("span", { class: "visually-hidden", text: (b.state || "idle") + " " }), words));
      }
      if (repos.length) {
        parts.push(el("span", { text: st.github_synced ? "github synced " + ago(st.github_synced) : "github not read yet" }));
      }
    }
    let live;
    if (state.connected) live = el("span", {}, el("span", { class: "live", "aria-hidden": "true", text: "●" }), " live");
    else live = el("span", { text: state.dropped ? "○ reconnecting…" : "○ connecting…" });
    parts.push(live);
    $("state").replaceChildren(...parts);
  }

  // --- Chat --------------------------------------------------------------

  function renderChat() {
    const box = $("messages");
    const lis = state.chat.map((m) => {
      const cls = m.role === "user" ? "user" : (m.kind === "line_jumper" ? "line_jumper" : "lasso");
      return el("li", { class: cls }, el("span", { class: "visually-hidden", text: m.role === "user" ? "You: " : "lasso: " }), m.text);
    });
    if (state.pending) {
      lis.push(el("li", { class: "user", text: state.pending }));
      lis.push(el("li", { class: "dim", text: "thinking…" }));
    }
    if (state.chatErr) lis.push(el("li", { class: "dim", text: "couldn't reach the assistant: " + state.chatErr }));
    if (!lis.length) {
      lis.push(el("li", { class: "dim", text: "Ask about anything on the tabs. I only speak up on my own when something can't wait." }));
    }
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    box.replaceChildren(...lis);
    if (atBottom) box.scrollTop = box.scrollHeight;
  }

  async function loadChat(open) {
    try {
      const out = open ? await api("POST", "/chat/open", {}) : await api("GET", "/chat");
      state.chat = out.messages || [];
      state.chatErr = "";
    } catch (e) {
      if (e instanceof SignedOut) return;
      state.chatErr = e.message;
    }
    renderChat();
  }

  async function sendChat(ev) {
    ev.preventDefault();
    const input = $("ask-input");
    const text = input.value.trim();
    if (!text || state.pending) return;
    input.value = "";
    state.pending = text;
    state.chatErr = "";
    renderChat();
    try {
      const out = await api("POST", "/chat", { text });
      state.chat = out.messages || [];
    } catch (e) {
      if (!(e instanceof SignedOut)) {
        state.chatErr = e.message;
        input.value = text; // give the words back
      }
    }
    state.pending = "";
    renderChat();
  }

  // --- Middle pane -------------------------------------------------------

  function renderTabs() {
    for (const b of $("tabs").querySelectorAll("button")) {
      const on = b.dataset.tab === state.tab;
      b.setAttribute("aria-selected", on ? "true" : "false");
      b.tabIndex = on ? 0 : -1;
    }
    $("rows").setAttribute("aria-labelledby", "tab-" + state.tab);
    $("hint").textContent = HINTS[state.tab];
  }

  function subtitle(it) {
    const parts = [];
    if (it.repo) parts.push(it.repo);
    if (!zeroTime(it.updated)) parts.push("updated " + ago(it.updated));
    return parts.join(" · ");
  }

  function renderRows() {
    const tab = state.tab;
    const items = state.items[tab];
    const box = $("rows");
    if (!state.loaded[tab]) {
      box.replaceChildren(el("p", { class: "dim", text: state.errs[tab] ? "couldn't load this tab: " + state.errs[tab] : "loading…" }));
      return;
    }
    if (!items.length) {
      box.replaceChildren(el("p", { class: "dim", text: "nothing here right now" }));
      return;
    }
    const sel = state.sel[tab];
    const lis = items.map((it) => {
      const cur = it.id === sel ? "true" : "false";
      let btn;
      if (tab === "summaries") {
        let foot = "last 24h";
        if (it.byline) foot += " · " + it.byline;
        if (!zeroTime(it.updated)) foot += (it.byline ? " at " : " · updated ") + hhmm(it.updated);
        btn = el("button", { class: "card", "aria-current": cur, onclick: () => select(it.id, true) },
          el("span", { class: "card-head", text: it.repo || it.title }),
          el("span", { class: "card-body md" }, markdown((it.body || "").trim() || it.title, true)),
          el("span", { class: "byline", text: foot }));
      } else {
        btn = el("button", { class: "row", "aria-current": cur, onclick: () => select(it.id, true) },
          el("span", { class: "row-line" },
            state.changed.has(it.id) ? el("span", { class: "changed", title: "changed", text: "• " }) : null,
            el("span", { class: "row-title", text: it.title }),
            it.right ? el("span", { class: "row-right", text: it.right }) : null),
          el("span", { class: "row-sub", text: subtitle(it) }));
      }
      return el("li", {}, btn);
    });
    box.replaceChildren(el("ul", { class: "rows" }, ...lis));
  }

  async function loadTab(tab) {
    try {
      const out = await api("GET", "/items?tab=" + encodeURIComponent(tab));
      state.items[tab] = out.items || [];
      state.loaded[tab] = true;
      state.errs[tab] = "";
    } catch (e) {
      if (e instanceof SignedOut) return;
      state.errs[tab] = e.message;
    }
    if (tab === state.tab) {
      renderRows();
      // Keep the focus pane on a row that still exists, or the first one.
      const ids = state.items[tab].map((it) => it.id);
      if (!ids.includes(state.sel[tab]) && ids.length) select(ids[0], false);
      else if (!ids.length) { state.sel[tab] = undefined; renderFocus(); }
    }
    renderHeader();
  }

  function setTab(tab, moveFocus) {
    if (!TABS.includes(tab)) return;
    state.tab = tab;
    renderTabs();
    renderRows();
    const id = state.sel[tab] || (state.items[tab][0] && state.items[tab][0].id);
    if (id) select(id, false);
    else { state.focus = null; renderFocus(); }
    if (moveFocus) $("tab-" + tab).focus();
  }

  // --- Focus pane --------------------------------------------------------

  let focusSeq = 0;

  async function select(id, byUser) {
    state.sel[state.tab] = id;
    state.changed.delete(id);
    renderRows();
    if (!state.focus || state.focus.id !== id) {
      state.focus = null;
      $("outcome").textContent = "";
    }
    renderFocus();
    const seq = ++focusSeq;
    try {
      const f = await api("GET", "/items/" + encodeURIComponent(id));
      if (seq !== focusSeq) return;
      state.focus = f;
    } catch (e) {
      if (e instanceof SignedOut || seq !== focusSeq) return;
      state.focus = { id, error: e.message };
    }
    renderFocus();
    if (byUser && window.matchMedia("(max-width: 900px)").matches) {
      $("focus").scrollIntoView({ block: "start" });
      $("focus").focus({ preventScroll: true });
    }
  }

  function selectedItem() {
    const id = state.sel[state.tab];
    return state.items[state.tab].find((it) => it.id === id);
  }

  function renderFocus() {
    const box = $("focus-body");
    const it = selectedItem();
    if (!it) {
      box.replaceChildren(el("p", { class: "dim", text: "nothing selected" }));
      return;
    }
    const meta = [it.repo, it.right].filter(Boolean);
    if (!zeroTime(it.updated)) meta.push("updated " + ago(it.updated));
    const out = [el("h3", { text: it.title }), el("p", { class: "meta", text: meta.join(" · ") })];
    const f = state.focus && state.focus.id === it.id ? state.focus : null;
    if (!f) {
      out.push(el("p", { class: "dim", text: "loading…" }));
    } else if (f.error) {
      out.push(el("p", { class: "dim", text: "couldn't load this item: " + f.error }));
    } else {
      if (f.fields && f.fields.length) {
        const dl = el("dl", { class: "fields" });
        for (const fd of f.fields) dl.append(el("dt", { text: fd.name }), el("dd", { text: fd.value }));
        out.push(dl);
      }
      let secs = f.sections || [];
      if (!secs.length && (f.body || "").trim()) secs = [{ body: f.body }];
      for (const s of secs) {
        out.push(el("div", { class: "section" },
          s.label ? el("h4", { class: "section-label", text: s.label }) : null,
          (s.body || "").trim() ? el("div", { class: "md" }, markdown(s.body)) : null));
      }
      if (f.take) out.push(el("p", { class: "take" }, "lasso · ", inline(f.take)));
      out.push(...renderActions(f, it));
    }
    box.replaceChildren(...out);
  }

  function renderActions(f, it) {
    const answers = [];
    const others = [];
    for (const a of f.actions || []) {
      if (a.kind === "open") {
        const url = it.url || f.url;
        if (url && /^https?:\/\//.test(url)) {
          others.push(el("a", { class: "button", href: url, target: "_blank", rel: "noopener noreferrer" }, a.label));
        }
        continue;
      }
      if (a.kind === "explain" || !a.request) continue; // the jargon glossary lives in the TUI for now
      const b = el("button", { type: "button", class: a.kind === "answer_prompt" ? "answer" : "", onclick: () => runAction(a, b) }, a.label);
      (a.kind === "answer_prompt" ? answers : others).push(b);
    }
    const out = [];
    if (answers.length) {
      out.push(el("h4", { class: "actions-head", text: "Answer" }), el("div", { class: "actions" }, ...answers));
    }
    if (others.length) out.push(el("div", { class: "actions" }, ...others));
    return out;
  }

  async function runAction(a, btn) {
    const buttons = $("focus-body").querySelectorAll("button");
    buttons.forEach((b) => { b.disabled = true; });
    $("outcome").textContent = "sending: " + a.label + "…";
    let msg;
    try {
      const out = await api("POST", "/actions", a.request);
      msg = (out && (out.detail || (out.decision && out.decision.reason))) || "done: " + a.label;
    } catch (e) {
      if (e instanceof SignedOut) return;
      msg = "couldn't " + a.label.toLowerCase() + ": " + e.message;
    }
    $("outcome").textContent = msg;
    buttons.forEach((b) => { b.disabled = false; });
    if (btn && document.body.contains(btn)) btn.focus();
  }

  // --- Markdown: bold, code and lists, built as DOM nodes so bot and
  // GitHub text can never become markup. ----------------------------------

  function inline(text) {
    const frag = document.createDocumentFragment();
    const re = /(\*\*[^*]+\*\*|`[^`]+`)/g;
    let last = 0;
    let m;
    while ((m = re.exec(text)) !== null) {
      if (m.index > last) frag.append(text.slice(last, m.index));
      const tok = m[0];
      if (tok.startsWith("**")) frag.append(el("strong", { text: tok.slice(2, -2) }));
      else frag.append(el("code", { text: tok.slice(1, -1) }));
      last = m.index + tok.length;
    }
    if (last < text.length) frag.append(text.slice(last));
    return frag;
  }

  function markdown(text, asSpans) {
    const frag = document.createDocumentFragment();
    const blocks = String(text).replace(/\r/g, "").split(/\n\s*\n/);
    for (const block of blocks) {
      const lines = block.split("\n").filter((l) => l.trim() !== "");
      if (!lines.length) continue;
      if (!asSpans && lines.every((l) => /^\s*[-*] /.test(l))) {
        frag.append(el("ul", {}, ...lines.map((l) => el("li", {}, inline(l.replace(/^\s*[-*] /, ""))))));
        continue;
      }
      // Inside a <button> (summary cards) only phrasing content is allowed.
      const p = el(asSpans ? "span" : "p", asSpans ? { class: "para" } : {});
      lines.forEach((l, i) => {
        if (i) p.append(el("br"));
        p.append(inline(l.replace(/^\s*[-*] /, "• ")));
      });
      frag.append(p);
    }
    return frag;
  }

  // --- Events ------------------------------------------------------------

  let events = null;
  let retry = null;

  function connect() {
    if (state.signedOut) return;
    events = new EventSource("/events");
    events.onopen = () => {
      const wasDropped = state.dropped;
      state.connected = true;
      renderHeader();
      if (wasDropped) refreshAll(); // anything missed while away
    };
    events.onerror = () => {
      state.connected = false;
      state.dropped = true;
      renderHeader();
      if (events.readyState === EventSource.CLOSED) {
        // The browser gave up (say, a 401): see why, then try again.
        clearTimeout(retry);
        retry = setTimeout(async () => {
          try { await api("GET", "/status"); connect(); } catch (e) { if (!(e instanceof SignedOut)) retry = setTimeout(connect, 3000); }
        }, 3000);
      }
    };
    const on = (kind, fn) => events.addEventListener(kind, (ev) => {
      let data = null;
      try { data = JSON.parse(ev.data); } catch (_) { return; }
      fn(data);
    });
    on("item.changed", (it) => {
      if (!TABS.includes(it.tab)) return;
      const synced = state.status && (state.status.github_synced || !hotRepos().length);
      if (state.loaded[it.tab] && synced && state.sel[it.tab] !== it.id) state.changed.add(it.id);
      loadTab(it.tab);
      if (state.focus && state.focus.id === it.id && state.sel[state.tab] === it.id) select(it.id, false);
    });
    on("item.removed", (d) => {
      state.changed.delete(d.id);
      for (const t of TABS) if (state.items[t].some((it) => it.id === d.id)) loadTab(t);
    });
    on("status.changed", (st) => {
      const repos = state.status && state.status.hot_repos;
      state.status = st;
      if (!st.hot_repos && repos) st.hot_repos = repos;
      renderHeader();
    });
    on("chat.updated", () => loadChat(false));
    on("focus", (d) => {
      if (!TABS.includes(d.tab)) return;
      state.tab = d.tab;
      renderTabs();
      state.sel[d.tab] = d.id;
      loadTab(d.tab);
      select(d.id, false);
    });
  }

  async function loadStatus() {
    try {
      state.status = await api("GET", "/status");
    } catch (e) {
      if (e instanceof SignedOut) return false;
    }
    renderHeader();
    return true;
  }

  function refreshAll() {
    loadStatus();
    TABS.forEach(loadTab);
    loadChat(false);
  }

  // --- Start -------------------------------------------------------------

  function wireTabs() {
    const tabs = $("tabs");
    tabs.addEventListener("click", (ev) => {
      const b = ev.target.closest("button[data-tab]");
      if (b) setTab(b.dataset.tab, false);
    });
    tabs.addEventListener("keydown", (ev) => {
      const i = TABS.indexOf(state.tab);
      let next = null;
      if (ev.key === "ArrowRight") next = TABS[(i + 1) % TABS.length];
      if (ev.key === "ArrowLeft") next = TABS[(i + TABS.length - 1) % TABS.length];
      if (ev.key === "Home") next = TABS[0];
      if (ev.key === "End") next = TABS[TABS.length - 1];
      if (next) { ev.preventDefault(); setTab(next, true); }
    });
  }

  async function start() {
    wireTabs();
    $("ask").addEventListener("submit", sendChat);
    renderTabs();
    renderRows();
    renderChat();
    renderHeader();
    if (!(await loadStatus())) return;
    TABS.forEach(loadTab);
    loadChat(true);
    connect();
    // Ages ("synced 4m ago") go stale; redraw them now and then.
    setInterval(() => {
      renderHeader();
      if (!$("rows").contains(document.activeElement)) renderRows(); // don't pull the keyboard off a row
    }, 30000);
  }

  document.addEventListener("DOMContentLoaded", start);
})();
