// Talking to the hub: JSON over fetch, with the header every state change
// needs (the hub refuses cross-site requests without it), and commands with
// a client-made ID that is reused when a command is retried, so a command
// sent twice is applied once.

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body || {};
    // network: the request did not reach the hub (offline, TLS refused).
    this.network = status === 0;
  }
}

async function request(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.headers['X-Devc-Request'] = '1';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    throw new ApiError(0, 'the hub cannot be reached');
  }
  let data = null;
  try {
    data = await res.json();
  } catch (e) {
    data = null;
  }
  if (!res.ok) {
    const msg = (data && data.error) || res.statusText || 'request failed';
    throw new ApiError(res.status, msg, data);
  }
  return data;
}

export const get = (path) => request('GET', path);
export const post = (path, body) => request('POST', path, body || {});

export function newID() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const x = [...b].map((v) => v.toString(16).padStart(2, '0')).join('');
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20)}`;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// command sends a command and returns the machine's receipt. Failures that
// may not have reached the machine (network, 502, 504) are retried with the
// same ID; the machine applies a command ID once.
export async function command(machine, type, params, wait = 0, id = newID()) {
  let last;
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      return await post('/api/commands', { machine, type, id, params, wait });
    } catch (e) {
      last = e;
      if (!(e.network || e.status === 502 || e.status === 504)) throw e;
      await sleep(800 * 2 ** attempt);
    }
  }
  throw last;
}

// receipt waits for a command's final receipt (up to wait seconds).
export const receipt = (machine, id, wait = 30) =>
  get(`/api/commands/${encodeURIComponent(machine)}/${encodeURIComponent(id)}?wait=${wait}`);
