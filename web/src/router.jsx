// A path router in a hundred lines. The dashboard has nine routes and no
// nested layouts; a routing library would be larger than the pages it routes.
//
// Query state is nuqs' job, not this file's. This tracks only the pathname,
// so a filter change re-renders a table and never re-runs a route match.

import { useCallback, useEffect, useSyncExternalStore } from "react";

export const BASE = "/dashboard";

const NAVIGATED = "drover:navigated";

function subscribe(onChange) {
  window.addEventListener("popstate", onChange);
  window.addEventListener(NAVIGATED, onChange);
  return () => {
    window.removeEventListener("popstate", onChange);
    window.removeEventListener(NAVIGATED, onChange);
  };
}

// usePathname re-renders only when the path part of the URL changes.
export function usePathname() {
  return useSyncExternalStore(
    subscribe,
    () => location.pathname,
    () => BASE,
  );
}

export function navigate(to, { replace = false } = {}) {
  if (replace) history.replaceState(null, "", to);
  else history.pushState(null, "", to);
  window.dispatchEvent(new Event(NAVIGATED));
}

export function href(...parts) {
  const tail = parts
    .filter((p) => p !== null && p !== undefined && p !== "")
    .map((p) => encodeURIComponent(String(p)))
    .join("/");
  return tail ? BASE + "/" + tail : BASE + "/";
}

// useSegments splits the path below /dashboard: ["activity", "<id>"].
export function useSegments() {
  const pathname = usePathname();
  let p = pathname;
  if (p.startsWith(BASE)) p = p.slice(BASE.length);
  return p
    .split("/")
    .filter(Boolean)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    });
}

// Link is an ordinary anchor. Middle-click, cmd-click and "copy link address"
// all have to keep working, so the href is real and only a plain left click
// is intercepted.
export function Link({ to, children, className, title, onClick }) {
  const go = useCallback(
    (e) => {
      if (onClick) onClick(e);
      if (e.defaultPrevented) return;
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
      e.preventDefault();
      if (to === location.pathname + location.search) return;
      navigate(to);
      window.scrollTo(0, 0);
    },
    [to, onClick],
  );
  return (
    <a href={to} className={className} title={title} onClick={go}>
      {children}
    </a>
  );
}

// useTitle names the browser tab, so a pinned dashboard tab says which page
// it is parked on.
export function useTitle(...parts) {
  const key = parts.filter(Boolean).join(" · ");
  useEffect(() => {
    document.title = key ? "drover · " + key : "drover";
  }, [key]);
}
