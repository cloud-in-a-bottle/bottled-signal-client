// Uploads a file in chunks, as the import API takes it: the OpenHost router buffers each request body in memory, so
// one request per 8MB. An interrupted upload continues from what the server has; a chunk that fails is retried with
// backoff (through network drops, a laptop waking from sleep, ...).
const CHUNK = 8 * 1024 * 1024;

// upload sends file to base (eg "/api/import"), continuing an upload of the same size if the server has one, and
// calls onProgress(received, total) as it goes.
export async function upload(api, base, file, onProgress) {
  let status = await api("GET", base);
  if (!(status.state === "uploading" && status.total === file.size)) {
    status = await api("POST", base, { size: file.size });
  }
  let offset = status.received;
  let failures = 0;
  while (offset < file.size) {
    onProgress(offset, file.size);
    const res = await fetch(`${base}/chunk?offset=${offset}`, {
      method: "PUT", body: file.slice(offset, offset + CHUNK), signal: AbortSignal.timeout(120_000),
    }).catch(() => null);
    if (res && (res.ok || res.status === 409)) {
      // 409: the server has a different amount (eg a retried chunk did arrive); it says where to continue.
      offset = (await res.json()).received;
      failures = 0;
    } else if (res && res.status < 500) {
      throw new Error((await res.json().catch(() => ({}))).detail || res.statusText);
    } else if (++failures > 10) {
      throw new Error("the upload keeps failing; choose the file again later to continue where it stopped");
    } else {
      await new Promise((r) => setTimeout(r, Math.min(30_000, 1000 * 2 ** failures)));
    }
  }
  onProgress(file.size, file.size);
}
