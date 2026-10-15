// The "Import history from a backup" section of settings: upload a zip of a phone's on-device backup (its
// SignalBackups folder) with the backup's recovery key.
import { el, formatSize } from "/static/ui.js";
import { upload } from "/static/app/upload.js";

function progressBar(fraction) {
  return el("div", { class: "progress" }, el("div", { style: `width:${Math.round(Math.min(1, fraction) * 100)}%` }));
}

function countsText(c) {
  return `${c.messages} messages in ${c.chats} chats, with ${c.attachments} attachments`
    + (c.skipped ? ` (${c.skipped} messages were already here)` : "");
}

// The settings section, for bottled-bridge-core's UI (web.Extension).
export function settingsSection({ api, on }) {
  const box = el("div");
  const section = el("section", {},
    el("h3", {}, "Import history from a backup"),
    el("p", { class: "hint" }, "Signal's servers don't keep history, so this server only has what arrived since it was "
      + "registered. To bring in what's on your phone: make an on-device backup (Settings › Chats › Backups), zip its ",
    el("code", {}, "SignalBackups"), " folder (on an iPhone: long-press it in Files › Compress), and upload the zip here "
      + "with the backup's 64-character recovery key. Messages already here aren't duplicated, so importing again later "
      + "is safe."),
    box);
  let uploading = false;
  const show = (...children) => box.replaceChildren(...children.filter(Boolean));
  const filePicker = (label) => el("label", { class: "button" }, label,
    el("input", { type: "file", accept: ".zip,application/zip", hidden: true, onchange: (e) => {
      if (e.target.files[0]) start(e.target.files[0]);
    } }));

  const render = (s) => {
    if (!box.isConnected || uploading) return;
    switch (s.state) {
      case "uploading":
        show(el("p", {}, `An upload stopped at ${formatSize(s.received)} of ${formatSize(s.total)}. `
          + "Choose the same file to continue it."),
        el("div", { class: "link-options" }, filePicker("Continue with file"),
          el("button", { type: "button", class: "link", onclick: discard }, "Cancel")));
        return;
      case "ready":
        return renderKey(s);
      case "importing":
        show(el("p", {}, `Importing… ${countsText(s.counts)} so far.`), progressBar(s.progress));
        return;
      case "failed":
        if (s.wrong_key) return renderKey(s);
        show(el("p", { class: "error" }, `Import failed: ${s.error}`), el("div", { class: "link-options" }, filePicker("Choose backup .zip")));
        return;
      case "done":
        show(el("p", {}, `Imported ${countsText(s.counts)}.`), el("div", { class: "link-options" }, filePicker("Import another backup")));
        return;
    }
    show(el("div", { class: "link-options" }, filePicker("Choose backup .zip")));
  };

  function renderKey(s) {
    const key = el("input", { name: "recovery_key", autocomplete: "off", spellcheck: "false", required: true,
      placeholder: "64 letters and digits, with or without spaces" });
    const go = el("button", { type: "submit" }, "Import");
    const form = el("form", { onsubmit: async (e) => {
      e.preventDefault();
      go.disabled = true;
      try {
        await api("POST", "/api/import/run", { recovery_key: key.value });
        // A small import can finish (and its event arrive) before this response, so read the status again.
        render(await api("GET", "/api/import"));
      } catch (err) { alert(err.message); go.disabled = false; }
    } },
    s.wrong_key ? el("p", { class: "error" }, `${s.error}. Check the key and try again; the backup doesn't need uploading again.`) : null,
    el("label", {}, "Recovery key", key),
    el("div", { class: "link-options" }, go, el("button", { type: "button", class: "link", onclick: discard }, "Cancel")));
    show(el("p", {}, `Uploaded ${formatSize(s.total)}.`), form);
  }

  async function start(file) {
    uploading = true;
    try {
      await upload(api, "/api/import", file, (received, total) => {
        show(el("p", {}, `Uploading ${formatSize(received)} of ${formatSize(total)}…`), progressBar(received / total));
      });
    } catch (err) {
      show(el("p", { class: "error" }, `Upload failed: ${err.message}`), el("div", { class: "link-options" }, filePicker("Choose backup .zip")));
      return;
    } finally {
      uploading = false;
    }
    render(await api("GET", "/api/import"));
  }

  async function discard() {
    try {
      await api("DELETE", "/api/import");
      render({ state: "idle" });
    } catch (err) { alert(err.message); }
  }

  on("import", render);
  api("GET", "/api/import").then(render, (err) => show(el("p", { class: "error" }, err.message)));
  return section;
}
