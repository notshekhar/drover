import { useCallback, useEffect, useRef, useState } from "react";

// useQuery fetches, then keeps fetching on an interval.
//
// The rule it exists to enforce: a refresh must never blank the screen. The
// old dashboard rebuilt its whole DOM every two seconds, which threw away
// scroll position and made every poll visible as a flicker. Here a reload
// keeps the last good data on screen until the new data lands, and a failed
// poll leaves the last good data up with the error beside it, because a
// dashboard that empties itself the moment the engine hiccups is a dashboard
// you stop trusting.
export function useQuery(fetcher, key, { interval = 0, enabled = true } = {}) {
  const [state, setState] = useState({ data: null, error: null, loading: enabled });
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!enabled) {
      setState((p) => ({ ...p, loading: false }));
      return;
    }
    const ctl = new AbortController();
    let alive = true;
    let timer = 0;

    const run = async () => {
      try {
        const data = await fetcherRef.current(ctl.signal);
        if (!alive) return;
        setState({ data, error: null, loading: false });
      } catch (err) {
        if (!alive || err.name === "AbortError") return;
        setState((p) => ({ data: p.data, error: err, loading: false }));
      }
    };

    setState((p) => ({ ...p, loading: true }));
    run();
    if (interval > 0) {
      timer = setInterval(() => {
        // A backgrounded tab polling an engine is pure waste, and coming back
        // to a stale page is fine because the next tick is a second away.
        if (document.visibilityState === "visible") run();
      }, interval);
    }
    return () => {
      alive = false;
      ctl.abort();
      if (timer) clearInterval(timer);
    };
  }, [key, interval, enabled, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { ...state, reload };
}

// useCopy copies text and reports it for a moment, so the button can say so.
export function useCopy() {
  const [copied, setCopied] = useState("");
  const copy = useCallback(async (text, label) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(label || text);
      setTimeout(() => setCopied(""), 1200);
    } catch {
      // Clipboard access can be refused; saying nothing is better than an
      // alert box on a dashboard.
    }
  }, []);
  return [copied, copy];
}
