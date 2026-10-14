const CHUNK = 8 * 1024 * 1024;

function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

// Wire the "import history" section of the settings modal.
export function openImportFlow(root, api) {
  const form = root.querySelector("#import-form");
  const statusBox = root.querySelector("#import-status");
  let selected = [];

  const select = (files) => {
    // Keep the layout below the chosen folder (dropping the folder's own name) and skip hidden files.
    let entries = [...files].map((f) => ({ file: f, path: f.webkitRelativePath.split("/").slice(1).join("/") }))
      .filter((x) => x.path && !x.path.split("/").some((p) => p.startsWith(".")));
    // The phone keeps a couple of snapshots side by side; only the newest complete one is needed.
    const snapshots = [...new Set(entries.map((x) => x.path.split("/")[0]))]
      .filter((d) => d.startsWith("signal-backup-") && !d.endsWith("-tmp")).sort();
    if (snapshots.length > 1) {
      const keep = snapshots[snapshots.length - 1];
      entries = entries.filter((x) => { const top = x.path.split("/")[0]; return !snapshots.includes(top) || top === keep; });
    }
    selected = entries;
    const total = selected.reduce((n, x) => n + x.file.size, 0);
    form.querySelector('[data-field="import-selection"]').textContent =
      `${selected.length} file${selected.length === 1 ? "" : "s"}, ${formatSize(total)}`;
    form.hidden = !selected.length;
  };
  root.querySelector("#import-folder").addEventListener("change", (e) => select(e.target.files, true));

  const showStatus = (s) => {
    const pct = Math.round(s.progress * 100);
    statusBox.replaceChildren();
    const p = document.createElement("p");
    p.textContent = s.state === "done"
      ? `Done: ${s.messages_imported} messages imported, ${s.messages_skipped} already present, ${s.attachments_imported} attachments.`
      : s.state === "failed" ? `Import failed: ${s.message}` : `${s.message} (${pct}%)`;
    if (s.state === "failed") p.className = "error";
    const bar = document.createElement("div");
    bar.className = "progress";
    bar.innerHTML = "<div></div>";
    bar.firstChild.style.width = `${pct}%`;
    statusBox.append(p);
    if (s.state === "importing" || s.state === "uploading") statusBox.append(bar);
  };

  let activeId = null;
  document.addEventListener("import-progress", (e) => {
    if (e.detail.id === activeId && root.isConnected) showStatus(e.detail);
  });

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const passphrase = form.passphrase.value.trim();
    const button = form.querySelector("button");
    button.disabled = true;
    try {
      const session = await api("POST", "/api/imports/");
      activeId = session.id;
      const total = selected.reduce((n, x) => n + x.file.size, 0);
      let sent = 0;
      for (const { file, path } of selected) {
        for (let offset = 0; offset < file.size || (offset === 0 && file.size === 0); offset += CHUNK) {
          const chunk = file.slice(offset, offset + CHUNK);
          const url = `/api/imports/${session.id}/chunk?path=${encodeURIComponent(path)}&offset=${offset}`;
          let attempt = 0;
          for (;;) {
            try {
              const res = await fetch(url, { method: "PUT", body: chunk });
              if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
              break;
            } catch (err) {
              if (++attempt >= 5) throw err;
              await new Promise((r) => setTimeout(r, 1000 * attempt));
            }
          }
          sent += chunk.size;
          showStatus({ state: "uploading", message: `Uploading ${formatSize(sent)} of ${formatSize(total)}`, progress: total ? sent / total : 1 });
          if (file.size === 0) break;
        }
      }
      showStatus(await api("POST", `/api/imports/${session.id}/start`, { passphrase }));
      form.hidden = true;
    } catch (err) {
      showStatus({ state: "failed", message: err.message, progress: 0 });
    } finally {
      button.disabled = false;
    }
  });
}
