function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  Object.assign(node, attrs);
  node.append(...children);
  return node;
}

// Wire the "move history to a phone" section of the settings modal.
export function openExportFlow(root, api) {
  const box = root.querySelector("#export-status");
  const button = root.querySelector('[data-action="export"]');

  const show = async (s) => {
    box.replaceChildren();
    button.disabled = !!s && s.state === "exporting";
    if (!s) return;
    if (s.state === "failed") {
      box.append(el("p", { className: "error", textContent: `Export failed: ${s.message}` }));
      return;
    }
    if (s.state === "exporting") {
      const bar = el("div", { className: "progress" }, el("div", {}));
      bar.firstChild.style.width = `${Math.round(s.progress * 100)}%`;
      box.append(el("p", { textContent: `${s.message}…` }), bar);
      return;
    }
    // The broadcast progress events leave out the recovery key; fetch the full status for it.
    if (!s.recovery_key) s = await api("GET", "/api/export/");
    if (!s || s.state !== "done") return;
    const key = s.recovery_key.match(/.{1,4}/g).join(" ");
    box.append(
      el("p", { textContent: `Ready: ${s.messages_exported} messages, ${s.attachments_exported} attachments, ${formatSize(s.size)}.` }),
      el("p", {}, el("a", { className: "button", href: `/api/export/${s.id}/SignalBackups.zip`, textContent: "Download SignalBackups.zip" })),
      el("p", { className: "hint", textContent: "Unzip it into the phone's storage (on iPhone: the Files app; on Android: a folder you can pick during restore), then in Signal's setup choose to restore from an on-device backup and enter this recovery key:" }),
      el("pre", { className: "recovery-key", textContent: key }),
      el("button", { type: "button", className: "link", textContent: "Delete this export", onclick: async () => {
        await api("DELETE", "/api/export/");
        show(null);
      } }),
    );
  };

  document.addEventListener("export-progress", (e) => { if (root.isConnected) show(e.detail); });
  button.addEventListener("click", async () => {
    try {
      show(await api("POST", "/api/export/"));
    } catch (err) {
      show({ state: "failed", message: err.message });
    }
  });
  api("GET", "/api/export/").then(show);
}
