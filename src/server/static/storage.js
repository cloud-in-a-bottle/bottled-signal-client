function formatSize(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i && n < 100 ? 1 : 0)} ${units[i]}`;
}

function formatDate(ms) {
  return new Date(ms).toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" });
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "style") node.style.cssText = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  node.append(...children.flat().filter((c) => c !== null && c !== undefined && c !== false));
  return node;
}

function pct(part, whole) {
  if (!whole) return "0%";
  const p = (part / whole) * 100;
  return p > 0 && p < 1 ? "<1%" : `${Math.round(p)}%`;
}

// Part-to-whole of the message history: one stacked bar, with a legend and the table carrying exact numbers.
// Categories arrive in a fixed order and map to color slots in that order, so a category keeps its color.
function breakdown(categories) {
  const total = categories.reduce((n, c) => n + c.bytes, 0);
  const tooltip = el("div", { class: "viz-tooltip", role: "tooltip", hidden: true });
  const bar = el("div", { class: "viz-stack", role: "img", "aria-label": "Storage by content type; exact values in the table below" });
  categories.forEach((c, i) => {
    if (!c.bytes) return;
    const seg = el("div", {
      class: "viz-seg",
      style: `flex-grow:${c.bytes};background:var(--series-${i + 1})`,
    });
    const show = (e) => {
      tooltip.replaceChildren(
        el("strong", {}, c.label),
        el("span", {}, `${formatSize(c.bytes)} · ${pct(c.bytes, total)}`),
        el("span", {}, `${c.count.toLocaleString()} ${c.key === "text" ? "messages" : "items"}`),
      );
      tooltip.hidden = false;
      const box = bar.getBoundingClientRect();
      const x = Math.min(Math.max(e.clientX - box.left, 70), box.width - 70);
      tooltip.style.left = `${x}px`;
    };
    seg.addEventListener("mousemove", show);
    seg.addEventListener("mouseleave", () => { tooltip.hidden = true; });
    bar.append(seg);
  });

  const legend = el("ul", { class: "viz-legend" }, categories.map((c, i) => el("li", {},
    el("span", { class: "viz-swatch", style: `background:var(--series-${i + 1})` }), c.label)));

  const table = el("table", { class: "viz-table" },
    el("thead", {}, el("tr", {}, el("th", {}, "Type"), el("th", {}, "Items"), el("th", {}, "Size"), el("th", {}, "Share"))),
    el("tbody", {}, categories.map((c, i) => el("tr", {},
      el("td", {}, el("span", { class: "viz-swatch", style: `background:var(--series-${i + 1})` }), c.label),
      el("td", {}, c.count.toLocaleString()),
      el("td", {}, formatSize(c.bytes)),
      el("td", {}, pct(c.bytes, total))))),
  );
  return el("div", { class: "viz-root" }, el("div", { class: "viz-stack-wrap" }, bar, tooltip), legend, table);
}

function largestList(items, close) {
  if (!items.length) return el("p", { class: "hint" }, "No attachments stored yet.");
  return el("ul", { class: "storage-largest" }, items.map((a) => {
    const type = a.content_type || "";
    const icon = () => el("span", { class: "storage-icon" },
      type.startsWith("image/") ? "🖼" : type.startsWith("video/") ? "🎞" : type.startsWith("audio/") ? "🎵" : "📄");
    const thumb = type.startsWith("image/")
      ? el("img", { src: `/api/attachments/${a.attachment_id}`, loading: "lazy", alt: "",
        onerror: (e) => e.target.replaceWith(icon()) })
      : icon();
    return el("li", {
      onclick: () => { close(); location.hash = `#/c/${a.conversation_id}/m/${a.message_id}`; },
      title: "Open in conversation",
    },
      el("div", { class: "storage-thumb" }, thumb),
      el("div", { class: "storage-meta" },
        el("strong", {}, a.filename || type || "attachment"),
        el("small", {}, `${a.conversation_name} · ${formatDate(a.sent_at)}`)),
      el("span", { class: "storage-size" }, formatSize(a.bytes)));
  }));
}

function conversationList(items, close) {
  const max = Math.max(1, ...items.map((c) => c.bytes));
  return el("ul", { class: "storage-convs" }, items.map((c) => el("li", {
    onclick: () => { close(); location.hash = `#/c/${c.conversation_id}`; },
  },
    el("div", { class: "storage-meta" },
      el("strong", {}, c.name),
      el("small", {}, `${c.messages.toLocaleString()} messages · ${c.attachments.toLocaleString()} attachments`)),
    el("span", { class: "storage-size" }, formatSize(c.bytes)),
    el("div", { class: "storage-bar" }, el("div", { style: `width:${(c.bytes / max) * 100}%` })))));
}

export async function openStorage(api) {
  const frag = document.getElementById("tpl-storage").content.cloneNode(true);
  const backdrop = frag.querySelector(".modal-backdrop");
  document.body.append(frag);
  const close = () => backdrop.remove();
  backdrop.querySelector('[data-action="close"]').addEventListener("click", close);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  const body = backdrop.querySelector("#storage-body");

  let r;
  try {
    r = await api("GET", "/api/storage");
  } catch (err) {
    body.replaceChildren(el("p", { class: "error" }, `Couldn't measure storage: ${err.message}`));
    return;
  }
  const total = r.attachment_bytes + r.database_bytes + r.signal_cli_bytes;
  body.replaceChildren(
    el("section", {},
      el("div", { class: "storage-hero" },
        el("div", {}, el("span", { class: "storage-hero-value" }, formatSize(total)), el("small", {}, "total on disk")),
        el("div", {}, el("span", { class: "storage-hero-value" }, r.messages.toLocaleString()),
          el("small", {}, `messages in ${r.conversations.toLocaleString()} chats`)),
        el("div", {}, el("span", { class: "storage-hero-value" }, r.attachments.toLocaleString()), el("small", {}, "attachments"))),
      el("p", { class: "hint" },
        `Attachments ${formatSize(r.attachment_bytes)} · message database ${formatSize(r.database_bytes)} · `
        + `Signal keys and state ${formatSize(r.signal_cli_bytes)}.`
        + (r.missing_attachments ? ` ${r.missing_attachments.toLocaleString()} attachments aren't stored (not in the backup they came from).` : ""))),
    el("section", {}, el("h3", {}, "By content type"), breakdown(r.categories)),
    el("section", {}, el("h3", {}, "Largest attachments"), largestList(r.largest_attachments, close)),
    el("section", {}, el("h3", {}, "Chats by size"), conversationList(r.top_conversations, close)),
  );
}
