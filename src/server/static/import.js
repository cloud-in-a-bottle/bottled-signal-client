// Backup zips are uploaded with tus (resumable uploads; window.tus is tus-js-client from /static/vendor). Chunks stay
// small because the OpenHost router buffers each request body in memory.
const CHUNK = 8 * 1024 * 1024;

function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

// tus-js-client retries failed requests but has no timeout, so a request that stalls (a dropped connection, a laptop
// waking from sleep) would wait forever. If nothing moves for this long, abort and restart; tus asks the server how
// much it already has and continues from there.
const STALL_MS = 60_000;

function upload(file, onProgress) {
  return new Promise((resolve, reject) => {
    let lastActivity = Date.now();
    let watchdog = null;
    const finish = (fn) => (value) => { clearInterval(watchdog); fn(value); };
    const up = new window.tus.Upload(file, {
      endpoint: "/api/uploads",
      chunkSize: CHUNK,
      // Keep retrying through network drops for several minutes before giving up.
      retryDelays: [0, 1000, 3000, 5000, 10_000, 20_000, 30_000, 30_000, 30_000, 30_000, 30_000],
      metadata: { filename: file.name, filetype: file.type || "application/zip" },
      // Lets a retried upload of the same file in this browser continue where it stopped.
      storeFingerprintForResuming: true,
      removeFingerprintOnSuccess: true,
      onProgress: (sent, total) => { lastActivity = Date.now(); onProgress(sent, total); },
      onError: finish(reject),
      onSuccess: finish(() => resolve(up.url.split("/").pop())),
    });
    watchdog = setInterval(() => {
      if (Date.now() - lastActivity < STALL_MS) return;
      lastActivity = Date.now();
      up.abort().then(() => up.start());
    }, 5000);
    up.findPreviousUploads().then((previous) => {
      if (previous.length) up.resumeFromPreviousUpload(previous[0]);
      up.start();
    }, finish(reject));
  });
}

// Wire the "import history" section of the settings modal.
export function openImportFlow(root, api) {
  const form = root.querySelector("#import-form");
  const statusBox = root.querySelector("#import-status");
  let selected = null;
  // An uploaded zip whose import failed only on the recovery key; retrying with a corrected key reuses it.
  let retryUploadId = null;

  root.querySelector("#import-file").addEventListener("change", (e) => {
    selected = e.target.files[0] || null;
    retryUploadId = null;
    form.querySelector('[data-field="import-selection"]').textContent =
      selected ? `${selected.name}, ${formatSize(selected.size)}` : "";
    form.hidden = !selected;
  });

  const showStatus = (s) => {
    const pct = Math.round(s.progress * 100);
    statusBox.replaceChildren();
    const p = document.createElement("p");
    p.textContent = s.state === "done"
      ? `Done: ${s.messages_imported} messages imported, ${s.messages_skipped} already present, ${s.attachments_imported} attachments.`
      : s.state === "failed" ? `Import failed: ${s.message}` : `${s.message} (${pct}%)`;
    if (s.state === "failed") p.className = "error";
    statusBox.append(p);
    if (s.state === "importing" || s.state === "uploading") {
      const bar = document.createElement("div");
      bar.className = "progress";
      bar.innerHTML = "<div></div>";
      bar.firstChild.style.width = `${pct}%`;
      statusBox.append(bar);
    }
  };

  let activeId = null;
  let sawProgress = false;
  document.addEventListener("import-progress", (e) => {
    if (e.detail.id !== activeId || !root.isConnected) return;
    sawProgress = true;
    if (e.detail.retry_with_new_key) {
      retryUploadId = e.detail.id;
      form.hidden = false;
      showStatus({ ...e.detail, message: `${e.detail.message}. Correct the key and press the button again; the zip won't need uploading again.` });
      return;
    }
    showStatus(e.detail);
  });

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const passphrase = form.passphrase.value.trim();
    const button = form.querySelector("button");
    button.disabled = true;
    try {
      let uploadId = retryUploadId;
      retryUploadId = null;
      if (!uploadId) {
        showStatus({ state: "uploading", message: "Checking free space", progress: 0 });
        await api("POST", "/api/imports/check-space", { total_bytes: selected.size });
        showStatus({ state: "uploading", message: "Starting upload", progress: 0 });
        uploadId = await upload(selected, (sent, total) => {
          showStatus({ state: "uploading", message: `Uploading ${formatSize(sent)} of ${formatSize(total)}`, progress: total ? sent / total : 0 });
        });
      }
      activeId = uploadId;
      sawProgress = false;
      form.hidden = true;
      const started = await api("POST", "/api/imports/", { upload_id: uploadId, passphrase });
      // Progress events can overtake this response (eg a wrong key fails fast); don't overwrite them.
      if (!sawProgress) showStatus(started);
    } catch (err) {
      form.hidden = false;
      const message = err.originalResponse ? `upload failed (${err.originalResponse.getStatus()})` : err.message;
      showStatus({ state: "failed", message: `${message}. Press Upload again to continue where it stopped.`, progress: 0 });
    } finally {
      button.disabled = false;
    }
  });
}
