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
      const total = selected.reduce((n, x) => n + x.file.size, 0);
      // Pick up an interrupted upload of the same folder: one whose files are all part of this selection and no
      // larger than the local copies.
      const sizes = new Map(selected.map((x) => [x.path, x.file.size]));
      const previous = (await api("GET", "/api/imports/resumable")).find((s) => {
        const got = Object.entries(s.received);
        return got.length > 0 && got.every(([path, size]) => sizes.has(path) && size <= sizes.get(path));
      });
      const received = previous ? previous.received : {};
      const session = previous || await api("POST", "/api/imports/", { total_bytes: total });
      activeId = session.id;
      let sent = selected.reduce((n, x) => n + Math.min(received[x.path] || 0, x.file.size), 0);
      const label = previous ? "Resuming upload" : "Uploading";
      for (const { file, path } of selected) {
        const done = received[path] || 0;
        if (done >= file.size && file.size > 0) continue;
        // Resume from the last whole chunk boundary; the server overwrites a partly-written chunk.
        for (let offset = done - (done % CHUNK); offset < file.size || (offset === 0 && file.size === 0); offset += CHUNK) {
          const chunk = file.slice(offset, offset + CHUNK);
          await putChunk(`/api/imports/${session.id}/chunk?path=${encodeURIComponent(path)}&offset=${offset}`, chunk);
          sent += Math.max(0, offset + chunk.size - Math.max(offset, done));
          showStatus({ state: "uploading", message: `${label}: ${formatSize(sent)} of ${formatSize(total)}`, progress: total ? sent / total : 1 });
          if (file.size === 0) break;
        }
      }
      showStatus(await api("POST", `/api/imports/${session.id}/start`, { passphrase }));
      form.hidden = true;
    } catch (err) {
      showStatus({
        state: "failed",
        message: `${err.message}. Choose the same folder and press Upload again to resume where it stopped.`,
        progress: 0,
      });
    } finally {
      button.disabled = false;
    }
  });
}

// Upload one chunk, giving up on a stalled request after CHUNK_TIMEOUT_MS and retrying with backoff for a few
// minutes, so a dropped connection or a laptop waking from sleep doesn't hang or kill the upload.
const CHUNK_TIMEOUT_MS = 120_000;
const RETRY_DELAYS_MS = [1000, 2000, 5000, 10_000, 20_000, 30_000, 30_000, 30_000, 30_000, 30_000];

async function putChunk(url, chunk) {
  for (let attempt = 0; ; attempt++) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), CHUNK_TIMEOUT_MS);
    try {
      const res = await fetch(url, { method: "PUT", body: chunk, signal: controller.signal });
      if (res.ok) return;
      const text = await res.text();
      let detail = text;
      try { detail = JSON.parse(text).detail || text; } catch {}
      // A rejected chunk (4xx) won't succeed on retry; anything else might.
      if (res.status >= 400 && res.status < 500) throw Object.assign(new Error(detail), { permanent: true });
      throw new Error(`server error ${res.status}`);
    } catch (err) {
      if (err.permanent || attempt >= RETRY_DELAYS_MS.length) {
        throw err.name === "AbortError" ? new Error("the upload stalled") : err;
      }
      await new Promise((r) => setTimeout(r, RETRY_DELAYS_MS[attempt]));
    } finally {
      clearTimeout(timer);
    }
  }
}
