export function humanDuration(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m";
  if (sec < 86400) {
    return Math.floor(sec / 3600) + "h" + Math.floor((sec % 3600) / 60) + "m";
  }
  return Math.floor(sec / 86400) + "d" + Math.floor((sec % 86400) / 3600) + "h";
}

export function relativeTime(iso) {
  if (!iso) return "never";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "never";
  const d = Date.now() - t.getTime();
  if (d < 5000) return "just now";
  if (d < 60_000) return Math.floor(d / 1000) + "s ago";
  if (d < 3_600_000) return Math.floor(d / 60_000) + "m ago";
  if (d < 86_400_000) return Math.floor(d / 3_600_000) + "h ago";
  return Math.floor(d / 86_400_000) + "d ago";
}

export function clock(iso) {
  if (!iso) return "";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "";
  return t.toLocaleTimeString(undefined, { hour12: false });
}

export function stamp(iso) {
  if (!iso) return "";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return String(iso);
  return t.toLocaleString(undefined, { hour12: false });
}

// dayOf groups rows into date headings without dragging in a date library.
export function dayOf(iso) {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "";
  const today = new Date();
  const y = new Date(today);
  y.setDate(y.getDate() - 1);
  const same = (a, b) => a.toDateString() === b.toDateString();
  if (same(t, today)) return "today";
  if (same(t, y)) return "yesterday";
  return t.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
}

export function shortCommit(c) {
  if (!c) return "-";
  return c.length > 8 ? c.slice(0, 8) : c;
}

export function shortRemote(url) {
  if (!url) return "";
  let s = url;
  for (const prefix of ["https://", "http://", "ssh://", "git://"]) {
    if (s.startsWith(prefix)) s = s.slice(prefix.length);
  }
  const slash = s.indexOf("/");
  const at = s.indexOf("@");
  if (at >= 0 && (slash < 0 || at < slash)) s = s.slice(at + 1);
  if (s.endsWith(".git")) s = s.slice(0, -4);
  return s;
}

// millis is deliberately coarse: a table of durations is read by shape, and
// three significant figures is as much as anybody compares by eye.
export function millis(n) {
  if (n === null || n === undefined || n === "") return "";
  const v = Number(n);
  if (!Number.isFinite(v)) return "";
  if (v < 1000) return v + "ms";
  if (v < 60_000) return (v / 1000).toFixed(v < 10_000 ? 1 : 0) + "s";
  return Math.floor(v / 60_000) + "m" + Math.round((v % 60_000) / 1000) + "s";
}

export function bytes(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v <= 0) return "";
  if (v < 1024) return v + " B";
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + " KB";
  return (v / (1024 * 1024)).toFixed(1) + " MB";
}
