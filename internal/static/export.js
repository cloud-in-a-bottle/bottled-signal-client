// The "Move history to a phone" section of settings: write everything here as an on-device backup a phone can
// restore during Signal's setup.
import { el, formatSize } from "/static/ui.js";

export function settingsSection({ api, on }) {
  const box = el("div");
  const section = el("section", {},
    el("h3", {}, "Move history to a phone"),
    el("p", { class: "hint" }, "Creates an on-device backup (a ", el("code", {}, "SignalBackups"), " folder, zipped) "
      + "that a phone can restore during Signal's setup, with this account's recovery key. ",
    el("strong", {}, "Restoring it moves the account back to that phone:"), " the phone becomes the primary device and "
      + "this server is signed out. Update notices (group changes, calls, ...) and messages about to disappear aren't "
      + "included, and text styles and mentions go as plain text."),
    box);
  const show = (...children) => box.replaceChildren(...children.filter(Boolean));
  const startButton = (label) => el("button", { type: "button", onclick: async (e) => {
    e.target.disabled = true;
    try {
      render(await api("POST", "/api/export"));
    } catch (err) { alert(err.message); e.target.disabled = false; }
  } }, label);

  const render = async (s) => {
    if (!box.isConnected) return;
    switch (s.state) {
      case "exporting":
        show(el("p", {}, `Writing the backup… ${s.counts.messages} messages, ${s.counts.attachments} attachments so far.`));
        return;
      case "failed":
        show(el("p", { class: "error" }, `Export failed: ${s.error}`), startButton("Try again"));
        return;
      case "done": {
        // Pushed events leave out the recovery key; the status asked for has it.
        if (!s.recovery_key) s = await api("GET", "/api/export");
        if (s.state !== "done") return render(s);
        show(
          el("p", {}, `Ready: ${s.counts.messages} messages and ${s.counts.attachments} attachments, ${formatSize(s.size)}.`),
          el("p", {}, el("a", { class: "button", href: `/api/export/${s.id}/SignalBackups.zip` }, "Download SignalBackups.zip")),
          el("p", { class: "hint" }, "Unzip it into the phone's storage (on an iPhone: the Files app; on Android: a folder "
            + "you can pick during restore), then in Signal's setup choose to restore from an on-device backup, and enter "
            + "this recovery key:"),
          el("pre", { class: "recovery-key" }, s.recovery_key.match(/.{1,4}/g).join(" ")),
          el("div", { class: "link-options" },
            el("button", { type: "button", class: "link", onclick: async () => {
              await api("DELETE", "/api/export").catch((err) => alert(err.message));
              render({ state: "idle" });
            } }, "Delete this backup")),
        );
        return;
      }
    }
    show(startButton("Create backup"));
  };

  on("export", render);
  api("GET", "/api/export").then(render, (err) => show(el("p", { class: "error" }, err.message)));
  return section;
}
