// The "Signal account" section of settings: the profile name others see, the PIN (registration lock), and the
// devices linked to this server.
import { el, formatTime, toast } from "/static/ui.js";

const LINK_PREFIX = "sgnl://linkdevice";

export function settingsSection({ api }) {
  const devices = el("ul", { class: "devices" });
  const number = el("strong", {}, "…");
  const linkBox = el("div");
  const section = el("section", {},
    el("h3", {}, "Signal account"),
    el("p", {}, "Registered as ", number),
    profileForm(api),
    pinForm(api),
    el("h3", {}, "Linked devices"),
    devices,
    el("details", {},
      el("summary", {}, "Link a device"),
      el("p", { class: "hint" }, "On the phone or computer, choose to link to an existing account so it shows a QR code. "
        + "Then scan it with this device's camera, upload a screenshot of it, or paste its ",
      el("code", {}, "sgnl://linkdevice"), " link. It gets your contacts and groups, and messages from then on, but not "
        + "past messages (signal-cli can't transfer them)."),
      linkBox));
  api("GET", "/api/account").then((a) => { number.textContent = a.number; }, (err) => { number.textContent = err.message; });
  const refresh = () => listDevices(api, devices);
  linkOptions(api, linkBox, refresh);
  refresh();
  return section;
}

function profileForm(api) {
  const given = el("input", { name: "given_name", placeholder: "First name", required: true, autocomplete: "given-name" });
  const family = el("input", { name: "family_name", placeholder: "Last name", autocomplete: "family-name" });
  return el("form", { class: "inline", onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/account/profile", { given_name: given.value, family_name: family.value });
      toast("Profile name updated.");
    } catch (err) { toast(err.message); }
  } }, given, family, el("button", { type: "submit" }, "Set profile name"));
}

function pinForm(api) {
  const pin = el("input", { name: "pin", type: "password", placeholder: "New Signal PIN", minlength: 4, required: true,
    autocomplete: "new-password" });
  return el("form", { class: "inline", onsubmit: async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/account/pin", { pin: pin.value });
      pin.value = "";
      toast("PIN set: registration lock is on.");
    } catch (err) { toast(err.message); }
  } }, pin, el("button", { type: "submit" }, "Set PIN"));
}

async function listDevices(api, list) {
  try {
    const devices = await api("GET", "/api/account/devices");
    list.replaceChildren(...devices.map((d) => el("li", {},
      el("span", {}, d.name || `Device ${d.id}`,
        el("small", {}, `Linked ${formatTime(d.createdTimestamp)} · last seen ${formatTime(d.lastSeenTimestamp)}`)),
      el("button", { type: "button", class: "link danger", onclick: async () => {
        if (!confirm(`Unlink ${d.name || `device ${d.id}`}?`)) return;
        await api("DELETE", `/api/account/devices/${d.id}`).catch((err) => toast(err.message));
        listDevices(api, list);
      } }, "Unlink"))));
    if (!devices.length) list.append(el("li", { class: "hint" }, "No linked devices."));
  } catch (err) {
    list.replaceChildren(el("li", { class: "error" }, err.message));
  }
}

function linkOptions(api, box, onLinked) {
  const video = el("video", { playsinline: true, muted: true, hidden: true });
  const uri = el("input", { name: "uri", placeholder: `${LINK_PREFIX}?uuid=…&pub_key=…`, required: true });
  let stopCamera = () => {};
  const link = async (value) => {
    stopCamera();
    try {
      await api("POST", "/api/account/devices", { uri: value });
      uri.value = "";
      toast("Linked. The device finishes setting up on its own screen.");
      onLinked();
    } catch (err) { toast(err.message); }
  };
  const decode = (blob) => fetch("/api/account/devices/decode-qr", { method: "POST", body: blob })
    .then(async (res) => (res.ok ? (await res.json()).uri : Promise.reject(new Error((await res.json()).detail))));
  box.append(
    el("div", { class: "link-options" },
      el("button", { type: "button", onclick: async () => {
        stopCamera = await scanWithCamera(video, decode, link);
      } }, "Scan with camera"),
      el("label", { class: "button" }, "Upload QR screenshot", el("input", { type: "file", accept: "image/*", hidden: true,
        onchange: async (e) => {
          const file = e.target.files[0];
          e.target.value = "";
          if (!file) return;
          try {
            await link(await decode(file));
          } catch (err) { toast(err.message); }
        } }))),
    video,
    el("form", { class: "inline", onsubmit: (e) => { e.preventDefault(); link(uri.value.trim()); } },
      uri, el("button", { type: "submit" }, "Link")));
}

// scanWithCamera looks for a device link QR code in the camera's picture: with the browser's own detector where there
// is one, else by sending frames to the server. It returns a function that stops the camera.
async function scanWithCamera(video, decode, onFound) {
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
    if (stopped || !video.isConnected) return stop();
    let found = null;
    try {
      if (detector) {
        const codes = await detector.detect(video);
        found = codes.map((c) => c.rawValue).find((v) => v.startsWith(LINK_PREFIX)) || null;
      } else {
        canvas.width = video.videoWidth;
        canvas.height = video.videoHeight;
        canvas.getContext("2d").drawImage(video, 0, 0);
        const blob = await new Promise((r) => canvas.toBlob(r, "image/jpeg", 0.85));
        found = await decode(blob).catch(() => null);
      }
    } catch {}
    if (found) {
      stop();
      onFound(found);
      return;
    }
    setTimeout(tick, detector ? 250 : 700);
  };
  tick();
  return stop;
}
