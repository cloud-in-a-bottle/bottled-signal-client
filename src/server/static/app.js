import { openExportFlow } from "/static/export.js";
import { openImportFlow } from "/static/import.js";
import { openStorage } from "/static/storage.js";

const app = document.getElementById("app");
const PAGE_SIZE = 50;
const REACTIONS = ["❤️", "👍", "👎", "😂", "😮", "😢"];

const state = {
  account: null,
  conversations: [],
  activeId: null,
  detail: null,
  messages: [],
  hasMore: false,
  replyTo: null,
  pendingFiles: [],
  highlight: null,
  typing: new Map(),
};

// ---------------------------------------------------------------- helpers

export async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    try {
      const data = await res.json();
      detail = data.detail || detail;
    } catch {}
    throw new Error(detail);
  }
  if (res.status === 204) return null;
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function template(id) {
  return document.getElementById(id).content.cloneNode(true);
}

function avatarColor(seed) {
  // Golden-angle spacing so consecutive ids get clearly different hues.
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return `hsl(${Math.round((h * 137.508) % 360)} 45% 48%)`;
}

function avatar(name, seed) {
  const initials = (name || "?").replace(/[^\p{L}\p{N} ]/gu, "").split(/\s+/).filter(Boolean)
    .slice(0, 2).map((w) => w[0].toUpperCase()).join("") || "#";
  return el("span", { class: "avatar", style: `background:${avatarColor(seed)}` }, initials);
}

function formatTime(ms) {
  const d = new Date(ms);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (now - d < 6 * 864e5) return d.toLocaleDateString([], { weekday: "short" });
  return d.toLocaleDateString([], { month: "short", day: "numeric", year: d.getFullYear() === now.getFullYear() ? undefined : "numeric" });
}

function formatDay(ms) {
  return new Date(ms).toLocaleDateString([], { weekday: "long", month: "long", day: "numeric", year: "numeric" });
}

function formatDuration(seconds) {
  if (!seconds) return "off";
  const units = [["w", 604800], ["d", 86400], ["h", 3600], ["m", 60], ["s", 1]];
  for (const [u, s] of units) if (seconds % s === 0) return `${seconds / s}${u}`;
  return `${seconds}s`;
}

function toast(message) {
  alert(message);
}

// ---------------------------------------------------------------- boot / setup

async function boot() {
  const s = await api("GET", "/api/state");
  state.account = s;
  if (!s.number) return renderSetup(s);
  renderMain();
}

function renderSetup(s) {
  app.className = "";
  app.replaceChildren(template("tpl-setup"));
  const register = app.querySelector('[data-step="register"]');
  const verify = app.querySelector('[data-step="verify"]');
  const error = app.querySelector("#setup-error");
  const showError = (e) => { error.textContent = e.message; error.hidden = false; };
  const showStep = (pending) => {
    register.hidden = !!pending;
    verify.hidden = !pending;
    app.querySelector('[data-field="pending"]').textContent = pending || "";
  };
  showStep(s.pending_number);
  if (!s.daemon_up) {
    error.textContent = "signal-cli is still starting…";
    error.hidden = false;
    setTimeout(boot, 2000);
  }

  app.querySelector("#register-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector("button");
    btn.disabled = true;
    error.hidden = true;
    try {
      await api("POST", "/api/register", {
        number: f.number.value, captcha: f.captcha.value, voice: f.voice.checked,
      });
      showStep(f.number.value);
    } catch (err) { showError(err); } finally { btn.disabled = false; }
  });
  app.querySelector("#verify-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector("button[type=submit]");
    btn.disabled = true;
    error.hidden = true;
    try {
      await api("POST", "/api/verify", { code: f.code.value, pin: f.pin.value || null });
      await boot();
    } catch (err) { showError(err); } finally { btn.disabled = false; }
  });
  app.querySelector('[data-action="restart"]').addEventListener("click", () => showStep(null));
}

// ---------------------------------------------------------------- main layout

function renderMain() {
  app.className = "";
  app.replaceChildren(template("tpl-main"));
  app.querySelector('[data-action="settings"]').addEventListener("click", openSettings);
  app.querySelector('[data-action="new-chat"]').addEventListener("click", openNewChat);
  app.querySelector('[data-action="storage"]').addEventListener("click", () => openStorage(api));
  const search = app.querySelector("#search");
  let searchTimer;
  search.addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(search.value.trim()), 250);
  });
  connectEvents();
  refreshConversations();
  window.addEventListener("hashchange", routeFromHash);
  routeFromHash();
}

function routeFromHash() {
  const m = location.hash.match(/^#\/c\/(\d+)(?:\/m\/(\d+))?/);
  if (m) openConversation(Number(m[1]), m[2] ? Number(m[2]) : null);
  else closeConversation();
}

async function refreshConversations() {
  state.conversations = await api("GET", "/api/conversations");
  renderConversationList();
}

function renderConversationList() {
  const list = app.querySelector("#conversations");
  if (!list) return;
  list.replaceChildren(...state.conversations.map((c) => el("li",
    { class: c.id === state.activeId ? "active" : "", onclick: () => { location.hash = `#/c/${c.id}`; } },
    avatar(c.name, c.recipient_id),
    el("span", { class: "name" }, c.name),
    el("span", { class: "time" }, c.last_message_at ? formatTime(c.last_message_at) : ""),
    el("span", { class: "preview" }, state.typing.has(c.id) ? "typing…" : (c.last_message_preview || "")),
    c.unread_count ? el("span", { class: "unread" }, c.unread_count) : el("span"),
  )));
  if (!state.conversations.length) {
    list.append(el("li", { class: "hint" }, "No conversations yet."));
  }
}

async function runSearch(q) {
  const results = app.querySelector("#search-results");
  const convs = app.querySelector("#conversations");
  if (!q) {
    results.hidden = true;
    convs.hidden = false;
    return;
  }
  const hits = await api("GET", `/api/search?q=${encodeURIComponent(q)}`);
  convs.hidden = true;
  results.hidden = false;
  results.replaceChildren(...hits.map((h) => el("li",
    { onclick: () => { location.hash = `#/c/${h.conversation_id}/m/${h.message_id}`; } },
    avatar(h.conversation_name, h.conversation_id),
    el("span", { class: "name" }, h.conversation_name),
    el("span", { class: "time" }, formatTime(h.sent_at)),
    el("span", { class: "preview" }, `${h.author_name}: `, ...highlightSnippet(h.snippet)),
  )));
  if (!hits.length) results.append(el("li", { class: "hint" }, "No matches."));
}

function highlightSnippet(snippet) {
  return snippet.split(/(\[[^\]]*\])/).map((part) =>
    part.startsWith("[") && part.endsWith("]") ? el("mark", {}, part.slice(1, -1)) : part);
}

// ---------------------------------------------------------------- conversation

function closeConversation() {
  state.activeId = null;
  const shell = app.querySelector(".shell");
  if (!shell) return;
  shell.classList.remove("in-thread");
  app.querySelector("#thread").replaceChildren(el("div", { class: "empty" }, "Select a conversation"));
  renderConversationList();
}

async function openConversation(id, highlightMessageId) {
  const switching = state.activeId !== id;
  state.activeId = id;
  state.highlight = highlightMessageId;
  app.querySelector(".shell").classList.add("in-thread");
  if (switching) {
    state.replyTo = null;
    state.pendingFiles = [];
    const thread = app.querySelector("#thread");
    thread.replaceChildren(template("tpl-thread"));
    wireThread(thread);
  }
  state.detail = await api("GET", `/api/conversations/${id}`);
  renderThreadHeader();
  if (highlightMessageId) {
    const loc = await api("GET", `/api/messages/${highlightMessageId}/location`);
    // Load a page ending just after the target so it is on screen.
    const page = await api("GET", `/api/conversations/${id}/messages?before=${loc.sent_at + 1}&limit=${PAGE_SIZE}`);
    const after = await api("GET", `/api/conversations/${id}/messages?limit=${PAGE_SIZE}`);
    state.messages = page.messages;
    state.hasMore = page.has_more;
    if (after.messages.length && after.messages[0].sent_at > loc.sent_at) {
      state.messages = state.messages.concat(after.messages.filter((m) => m.sent_at > loc.sent_at));
    }
  } else {
    const page = await api("GET", `/api/conversations/${id}/messages?limit=${PAGE_SIZE}`);
    state.messages = page.messages;
    state.hasMore = page.has_more;
  }
  renderMessages({ scroll: highlightMessageId ? "highlight" : "bottom" });
  renderConversationList();
  markRead();
}

function renderThreadHeader() {
  const d = state.detail;
  const thread = app.querySelector("#thread");
  thread.querySelector('[data-field="name"]').textContent = d.summary.name;
  const parts = [];
  if (d.summary.kind === "group") parts.push(`${d.members.length} members`);
  else if (d.summary.e164) parts.push(d.summary.e164);
  if (d.summary.expire_timer_s) parts.push(`⏱ ${formatDuration(d.summary.expire_timer_s)}`);
  thread.querySelector('[data-field="subtitle"]').textContent = parts.join(" · ");
}

function wireThread(thread) {
  thread.querySelector('[data-action="back"]').addEventListener("click", () => { location.hash = ""; });
  thread.querySelector('[data-action="load-more"]').addEventListener("click", loadMore);
  thread.querySelector('[data-action="cancel-reply"]').addEventListener("click", () => setReply(null));
  const input = thread.querySelector("#composer-input");
  const fileInput = thread.querySelector("#file-input");
  let typingSent = 0;
  let typingStopTimer;
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = `${input.scrollHeight}px`;
    const now = Date.now();
    if (input.value && now - typingSent > 8000) {
      typingSent = now;
      api("POST", `/api/conversations/${state.activeId}/typing`, { stop: false }).catch(() => {});
    }
    clearTimeout(typingStopTimer);
    typingStopTimer = setTimeout(() => {
      if (typingSent) {
        typingSent = 0;
        api("POST", `/api/conversations/${state.activeId}/typing`, { stop: true }).catch(() => {});
      }
    }, 4000);
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && window.matchMedia("(min-width: 761px)").matches) {
      e.preventDefault();
      thread.querySelector("#composer").requestSubmit();
    }
  });
  input.addEventListener("paste", (e) => {
    const files = [...e.clipboardData.files];
    if (files.length) {
      e.preventDefault();
      addPendingFiles(files);
    }
  });
  fileInput.addEventListener("change", () => {
    addPendingFiles([...fileInput.files]);
    fileInput.value = "";
  });
  thread.querySelector("#composer").addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = input.value.trim();
    if (!body && !state.pendingFiles.length) return;
    const form = new FormData();
    form.append("body", body);
    if (state.replyTo) form.append("quote_id", String(state.replyTo.id));
    for (const f of state.pendingFiles) form.append("files", f, f.name);
    const btn = thread.querySelector(".send");
    btn.disabled = true;
    try {
      await api("POST", `/api/conversations/${state.activeId}/messages`, form);
      input.value = "";
      input.style.height = "auto";
      typingSent = 0;
      setReply(null);
      state.pendingFiles = [];
      renderPendingFiles();
    } catch (err) {
      toast(`Send failed: ${err.message}`);
    } finally {
      btn.disabled = false;
      input.focus();
    }
  });
}

function addPendingFiles(files) {
  state.pendingFiles.push(...files);
  renderPendingFiles();
}

function renderPendingFiles() {
  const box = app.querySelector("#pending-files");
  if (!box) return;
  box.hidden = !state.pendingFiles.length;
  box.replaceChildren(...state.pendingFiles.map((f, i) => el("span", {},
    f.name, " ", el("button", { type: "button", class: "icon", onclick: () => {
      state.pendingFiles.splice(i, 1);
      renderPendingFiles();
    } }, "✕"))));
}

function setReply(message) {
  state.replyTo = message;
  const bar = app.querySelector("#reply-bar");
  if (!bar) return;
  bar.hidden = !message;
  if (message) {
    bar.querySelector('[data-field="reply-author"]').textContent = message.author_name;
    bar.querySelector('[data-field="reply-text"]').textContent = message.body || "(attachment)";
    app.querySelector("#composer-input").focus();
  }
}

async function loadMore() {
  if (!state.messages.length) return;
  const page = await api("GET",
    `/api/conversations/${state.activeId}/messages?before=${state.messages[0].sent_at}&limit=${PAGE_SIZE}`);
  state.messages = page.messages.concat(state.messages);
  state.hasMore = page.has_more;
  renderMessages({ scroll: "keep" });
}

async function reloadActive() {
  if (state.activeId === null) return;
  const newest = await api("GET", `/api/conversations/${state.activeId}/messages?limit=${PAGE_SIZE}`);
  // Replace the newest page in place, keeping any older messages already loaded.
  const oldestNew = newest.messages.length ? newest.messages[0].sent_at : Infinity;
  const older = state.messages.filter((m) => m.sent_at < oldestNew);
  state.messages = older.concat(newest.messages);
  if (!older.length) state.hasMore = newest.has_more;
  const box = app.querySelector("#messages");
  const atBottom = box && box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  renderMessages({ scroll: atBottom ? "bottom" : "keep" });
  if (document.visibilityState === "visible") markRead();
}

let markReadTimer;
function markRead() {
  clearTimeout(markReadTimer);
  const id = state.activeId;
  markReadTimer = setTimeout(() => {
    if (id === state.activeId) api("POST", `/api/conversations/${id}/read`).catch(() => {});
  }, 500);
}

// ---------------------------------------------------------------- messages

function renderMessages({ scroll }) {
  const box = app.querySelector("#messages");
  const list = app.querySelector("#message-list");
  if (!box || !list) return;
  const prevHeight = box.scrollHeight;
  const prevTop = box.scrollTop;
  app.querySelector('[data-action="load-more"]').hidden = !state.hasMore;
  const isGroup = state.detail && state.detail.summary.kind === "group";
  const items = [];
  let lastDay = null;
  let lastAuthor = null;
  for (const m of state.messages) {
    const day = new Date(m.sent_at).toDateString();
    if (day !== lastDay) {
      items.push(el("li", { class: "day" }, formatDay(m.sent_at)));
      lastDay = day;
      lastAuthor = null;
    }
    if (m.kind === "update") {
      items.push(el("li", { class: "update" }, describeUpdate(m), " · ", formatTime(m.sent_at)));
      lastAuthor = null;
      continue;
    }
    const showAuthor = isGroup && !m.outgoing && lastAuthor !== m.author_id;
    items.push(renderMessage(m, showAuthor));
    lastAuthor = m.author_id;
  }
  list.replaceChildren(...items);
  if (scroll === "bottom") box.scrollTop = box.scrollHeight;
  else if (scroll === "keep") box.scrollTop = prevTop + (box.scrollHeight - prevHeight);
  else if (scroll === "highlight") {
    const target = list.querySelector(".msg.highlight");
    if (target) target.scrollIntoView({ block: "center" });
  }
}

function describeUpdate(m) {
  const p = m.update_payload ? JSON.parse(m.update_payload) : {};
  const who = m.outgoing ? "You" : m.author_name;
  switch (p.type) {
    case "groupUpdate": return `${who} updated the group${p.groupName ? ` “${p.groupName}”` : ""}`;
    case "expirationTimer": return `${who} set disappearing messages to ${formatDuration(p.seconds)}`;
    case "groupCall": return "Group call";
    case "call": return `${p.direction === "outgoing" ? "Outgoing" : "Incoming"} ${p.video ? "video" : "voice"} call${p.missed ? " (missed)" : ""}`;
    case "profileChange": return `${who} changed their profile name`;
    case "safetyNumberChange": return `Safety number with ${who} changed`;
    case "chatSessionRefresh": return "Chat session refreshed";
    case "joinedSignal": return `${who} is on Signal`;
    default: return p.text || "Conversation updated";
  }
}

function renderMessage(m, showAuthor) {
  const mine = (r) => r.reactor_name === "You";
  const node = el("li", { class: `msg${m.outgoing ? " out" : ""}${m.id === state.highlight ? " highlight" : ""}` });
  if (showAuthor) node.append(el("span", { class: "author" }, m.author_name));
  const bubble = el("div", { class: "bubble" });
  if (m.deleted) {
    bubble.classList.add("deleted");
    bubble.append(m.outgoing ? "You deleted this message." : "This message was deleted.");
  } else {
    if (m.quote) {
      bubble.append(el("div", {
        class: "quote",
        onclick: () => { if (m.quote.message_id) location.hash = `#/c/${state.activeId}/m/${m.quote.message_id}`; },
      }, el("strong", {}, m.quote.author_name), m.quote.text || "(attachment)"));
    }
    if (m.view_once) {
      bubble.append(renderViewOnce(m));
    } else {
      for (const a of m.attachments) bubble.append(renderAttachment(a));
    }
    if (m.body) bubble.append(renderBody(m.body, m.body_ranges));
  }
  node.append(bubble);
  if (m.reactions.length) {
    const groups = new Map();
    for (const r of m.reactions) {
      if (!groups.has(r.emoji)) groups.set(r.emoji, []);
      groups.get(r.emoji).push(r);
    }
    node.append(el("div", { class: "reactions" }, [...groups].map(([emoji, rs]) => el("span", {
      class: rs.some(mine) ? "mine" : "", title: rs.map((r) => r.reactor_name).join(", "),
    }, emoji, rs.length > 1 ? ` ${rs.length}` : ""))));
  }
  const meta = [formatTime(m.sent_at)];
  if (m.edited_at) meta.push("edited");
  if (m.expires_at) meta.push(`⏱ ${formatDuration(Math.max(0, Math.round((m.expires_at - Date.now()) / 60000)) * 60) || "<1m"}`);
  if (m.status) meta.push({ sent: "✓", delivered: "✓✓", read: "✓✓ read", viewed: "✓✓ viewed" }[m.status]);
  node.append(el("div", { class: "meta" }, meta.join(" · ")));
  if (!m.deleted) node.append(renderActions(m));
  node.addEventListener("click", (e) => {
    if (window.matchMedia("(max-width: 760px)").matches && !e.target.closest("a,button,audio,video,img,.quote")) {
      node.classList.toggle("show-actions");
    }
  });
  return node;
}

function renderActions(m) {
  const myReaction = m.reactions.find((r) => r.reactor_name === "You");
  const actions = el("div", { class: "actions" });
  const react = (emoji) => api("POST", `/api/messages/${m.id}/reaction`, { emoji }).catch((e) => toast(e.message));
  actions.append(
    el("span", { class: "emoji-picker" }, REACTIONS.map((emoji) => el("button", {
      title: `React ${emoji}`,
      onclick: () => react(myReaction && myReaction.emoji === emoji ? null : emoji),
    }, emoji))),
    el("button", { title: "Reply", onclick: () => setReply(m) }, "↩"),
  );
  if (m.outgoing) {
    actions.append(el("button", {
      title: "Delete for everyone",
      onclick: async () => {
        if (!confirm("Delete this message for everyone?")) return;
        await api("DELETE", `/api/messages/${m.id}`).catch((e) => toast(e.message));
      },
    }, "🗑"));
  }
  return actions;
}

function renderViewOnce(m) {
  if (m.viewed || m.outgoing || !m.attachments.some((a) => a.status === "present")) {
    return el("span", { class: "missing" }, m.outgoing ? "View-once media" : "Viewed");
  }
  return el("button", {
    class: "view-once",
    onclick: () => {
      const overlay = el("div", { class: "modal-backdrop", onclick: async () => {
        overlay.remove();
        await api("POST", `/api/messages/${m.id}/view-once-opened`).catch((e) => toast(e.message));
      } }, ...m.attachments.map(renderAttachment));
      document.body.append(overlay);
    },
  }, "▶ View once");
}

function renderAttachment(a) {
  const url = `/api/attachments/${a.id}`;
  const type = a.content_type || "";
  if (a.status !== "present") {
    const label = a.status === "purged" ? "Media removed" : "Attachment not available";
    return el("div", { class: "missing" }, `${label}${a.filename ? ` (${a.filename})` : ""}`);
  }
  let media;
  // SVG can carry script, so the server only offers it as a download.
  if (type.startsWith("image/") && !type.startsWith("image/svg")) {
    media = el("a", { href: url, target: "_blank", rel: "noopener" },
      el("img", { src: url, loading: "lazy", alt: a.filename || "image", width: a.width, height: a.height }));
  } else if (type.startsWith("video/")) {
    media = el("video", { src: url, controls: true, preload: "metadata" });
  } else if (type.startsWith("audio/")) {
    media = el("audio", { src: url, controls: true, preload: "metadata" });
  } else {
    media = el("a", { class: "file", href: url, download: a.filename || "" },
      `📄 ${a.filename || "file"}${a.size ? ` (${formatSize(a.size)})` : ""}`);
  }
  return a.caption ? el("div", {}, media, el("div", {}, a.caption)) : media;
}

function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

const URL_RE = /\bhttps?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g;

function linkify(text) {
  const out = [];
  let last = 0;
  for (const match of text.matchAll(URL_RE)) {
    if (match.index > last) out.push(text.slice(last, match.index));
    out.push(el("a", { href: match[0], target: "_blank", rel: "noopener noreferrer" }, match[0]));
    last = match.index + match[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

// Signal ranges are in UTF-16 code units, which is also how JS indexes strings.
function renderBody(body, rawRanges) {
  const ranges = rawRanges ? JSON.parse(rawRanges) : [];
  const span = el("div", { class: "text" });
  if (!ranges.length) {
    span.append(...linkify(body));
    return span;
  }
  const points = new Set([0, body.length]);
  for (const r of ranges) { points.add(r.start); points.add(Math.min(body.length, r.start + r.length)); }
  const sorted = [...points].sort((a, b) => a - b);
  for (let i = 0; i < sorted.length - 1; i++) {
    const [from, to] = [sorted[i], sorted[i + 1]];
    const active = ranges.filter((r) => r.start <= from && r.start + r.length >= to);
    const mention = active.find((r) => r.mentionAci);
    let text = body.slice(from, to);
    if (mention) {
      const member = state.detail && state.detail.members.find((x) => x.aci === mention.mentionAci);
      text = text.replace("￼", `@${member ? member.name : "mention"}`);
    }
    const classes = active.filter((r) => r.style).map((r) => `style-${r.style}`);
    if (mention) classes.push("mention");
    if (!classes.length) { span.append(...linkify(text)); continue; }
    const part = el("span", { class: classes.join(" ") }, text);
    if (classes.includes("style-SPOILER")) part.addEventListener("click", () => part.classList.add("revealed"));
    span.append(part);
  }
  return span;
}

// ---------------------------------------------------------------- live events

function connectEvents() {
  const source = new EventSource("/api/events");
  let refreshTimer;
  const refreshSoon = () => {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refreshConversations, 150);
  };
  source.addEventListener("conversation", (e) => {
    const { conversationId } = JSON.parse(e.data);
    state.typing.delete(conversationId);
    if (conversationId === state.activeId) reloadActive();
    refreshSoon();
  });
  source.addEventListener("conversations", () => {
    refreshSoon();
    if (state.activeId !== null) reloadActive();
  });
  source.addEventListener("typing", (e) => {
    const { conversationId, started } = JSON.parse(e.data);
    clearTimeout(state.typing.get(conversationId));
    if (started) state.typing.set(conversationId, setTimeout(() => {
      state.typing.delete(conversationId);
      renderTyping();
      renderConversationList();
    }, 15000));
    else state.typing.delete(conversationId);
    renderTyping();
    renderConversationList();
  });
  source.addEventListener("export", (e) => {
    document.dispatchEvent(new CustomEvent("export-progress", { detail: JSON.parse(e.data) }));
  });
  source.addEventListener("import", (e) => {
    document.dispatchEvent(new CustomEvent("import-progress", { detail: JSON.parse(e.data) }));
  });
  // EventSource reconnects on its own; resync once it does in case events were missed.
  source.addEventListener("hello", () => {
    refreshSoon();
    if (state.activeId !== null) reloadActive();
  });
}

function renderTyping() {
  const box = app.querySelector("#typing");
  if (!box) return;
  const typing = state.typing.has(state.activeId);
  box.hidden = !typing;
  box.textContent = typing ? "typing…" : "";
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && state.activeId !== null) markRead();
});

// ---------------------------------------------------------------- new chat

async function openNewChat() {
  const frag = template("tpl-new-chat");
  const backdrop = frag.querySelector(".modal-backdrop");
  document.body.append(frag);
  const close = () => backdrop.remove();
  backdrop.querySelector('[data-action="close"]').addEventListener("click", close);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  const start = async (payload) => {
    try {
      const ref = await api("POST", "/api/conversations", payload);
      close();
      await refreshConversations();
      location.hash = `#/c/${ref.id}`;
    } catch (err) { toast(err.message); }
  };
  backdrop.querySelector("#new-chat-form").addEventListener("submit", (e) => {
    e.preventDefault();
    start({ number: e.target.number.value });
  });
  const contacts = await api("GET", "/api/contacts");
  const list = backdrop.querySelector("#contacts");
  const render = (filter) => {
    const f = filter.toLowerCase();
    list.replaceChildren(...contacts
      .filter((c) => !f || c.name.toLowerCase().includes(f) || (c.e164 || "").includes(f))
      .map((c) => el("li", { onclick: () => start({ recipient_id: c.recipient_id }) },
        avatar(c.name, c.recipient_id), el("span", {}, c.name, c.e164 ? el("small", { class: "hint" }, ` ${c.e164}`) : ""))));
  };
  backdrop.querySelector("#contact-filter").addEventListener("input", (e) => render(e.target.value));
  render("");
}

// ---------------------------------------------------------------- settings

async function openSettings() {
  const frag = template("tpl-settings");
  const backdrop = frag.querySelector(".modal-backdrop");
  document.body.append(frag);
  let stopCamera = () => {};
  const close = () => { stopCamera(); backdrop.remove(); };
  backdrop.querySelector('[data-action="close"]').addEventListener("click", close);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector('[data-field="number"]').textContent = state.account.number;

  backdrop.querySelector("#profile-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/profile", {
        given_name: e.target.given_name.value, family_name: e.target.family_name.value || null,
      });
      toast("Profile name updated.");
    } catch (err) { toast(err.message); }
  });
  backdrop.querySelector("#pin-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/pin", { pin: e.target.pin.value });
      e.target.reset();
      toast("PIN set. Registration lock is now on for this number.");
    } catch (err) { toast(err.message); }
  });

  const devicesList = backdrop.querySelector("#devices");
  const loadDevices = async () => {
    try {
      const devices = await api("GET", "/api/devices");
      devicesList.replaceChildren(...devices.map((d) => el("li", {},
        el("span", {}, d.id === 1 ? "This server (primary)" : (d.name || `Device ${d.id}`),
          el("small", {}, d.last_seen_at ? `Last seen ${formatTime(d.last_seen_at)}` : "")),
        d.id === 1 ? null : el("button", { class: "danger", onclick: async () => {
          if (!confirm(`Unlink ${d.name || `device ${d.id}`}?`)) return;
          await api("DELETE", `/api/devices/${d.id}`).catch((err) => toast(err.message));
          loadDevices();
        } }, "Unlink"))));
    } catch (err) {
      devicesList.replaceChildren(el("li", { class: "error" }, err.message));
    }
  };
  loadDevices();

  const linkForm = backdrop.querySelector("#link-form");
  const link = async (uri) => {
    linkForm.uri.value = uri;
    stopCamera();
    try {
      await api("POST", "/api/devices", { uri });
      linkForm.reset();
      toast("Device linked.");
      loadDevices();
    } catch (err) { toast(`Linking failed: ${err.message}`); }
  };
  linkForm.addEventListener("submit", (e) => { e.preventDefault(); link(linkForm.uri.value); });
  backdrop.querySelector("#qr-file").addEventListener("change", async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    const form = new FormData();
    form.append("image", file);
    try {
      const { uri } = await api("POST", "/api/devices/decode-qr", form);
      if (confirm("Link this device now?")) link(uri);
      else linkForm.uri.value = uri;
    } catch (err) { toast(err.message); }
    e.target.value = "";
  });
  backdrop.querySelector('[data-action="scan-camera"]').addEventListener("click", async () => {
    stopCamera();
    stopCamera = await scanWithCamera(backdrop.querySelector("#qr-video"), (uri) => {
      if (confirm("Link this device now?")) link(uri);
      else linkForm.uri.value = uri;
    });
  });

  openImportFlow(backdrop, api);
  openExportFlow(backdrop, api);
}

async function scanWithCamera(video, onFound) {
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: "environment" } });
  } catch (err) {
    toast(`Camera unavailable: ${err.message}`);
    return () => {};
  }
  video.srcObject = stream;
  video.hidden = false;
  await video.play();
  let stopped = false;
  const stop = () => {
    stopped = true;
    stream.getTracks().forEach((t) => t.stop());
    video.hidden = true;
  };
  const detector = "BarcodeDetector" in window ? new window.BarcodeDetector({ formats: ["qr_code"] }) : null;
  const canvas = document.createElement("canvas");
  const tick = async () => {
    if (stopped) return;
    try {
      let uri = null;
      if (detector) {
        const codes = await detector.detect(video);
        uri = codes.map((c) => c.rawValue).find((v) => v.startsWith("sgnl://linkdevice")) || null;
      } else {
        // No native detector (eg Firefox): let the server decode a frame.
        canvas.width = video.videoWidth;
        canvas.height = video.videoHeight;
        canvas.getContext("2d").drawImage(video, 0, 0);
        const blob = await new Promise((r) => canvas.toBlob(r, "image/jpeg", 0.85));
        const form = new FormData();
        form.append("image", blob, "frame.jpg");
        uri = await api("POST", "/api/devices/decode-qr", form).then((r) => r.uri, () => null);
      }
      if (uri) { stop(); onFound(uri); return; }
    } catch {}
    setTimeout(tick, detector ? 250 : 700);
  };
  tick();
  return stop;
}

boot().catch((err) => {
  app.className = "loading";
  app.textContent = `Failed to load: ${err.message}`;
});
