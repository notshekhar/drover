// The engine's JSON API. Every call is a GET; the dashboard reads and does
// not write, so there is no CSRF surface here to think about.

const PREFIX = "/apis/drover/v1";

async function get(path, signal) {
  const res = await fetch(path, { signal });
  if (!res.ok) {
    let msg = res.statusText || `HTTP ${res.status}`;
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch {
      // keep statusText
    }
    throw new Error(msg);
  }
  return res.json();
}

// query drops empty values, so an untouched filter never reaches the engine.
function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v === null || v === undefined || v === "" || v === false) continue;
    q.set(k, String(v));
  }
  const s = q.toString();
  return s ? "?" + s : "";
}

export function dashboard(signal) {
  return get(PREFIX + "/dashboard", signal);
}

export function activity(params, signal) {
  return get(PREFIX + "/activity" + query(params), signal);
}

export function activityStats(params, signal) {
  return get(PREFIX + "/activity/stats" + query(params), signal);
}

export function activityOne(id, signal) {
  return get(PREFIX + "/activity/" + encodeURIComponent(id), signal);
}

export function object(plural, name, signal) {
  return get(PREFIX + "/" + plural + "/" + encodeURIComponent(name), signal);
}
