// The dashboard's vocabulary: marks, chips, sections, tables.
//
// It borrows the TUI's grammar on purpose. The same dot means the same thing
// in `drover dash` and in the browser, so the two read as one product and
// nobody has to learn a second set of symbols. No emoji, no icon fonts.

import { useEffect, useRef } from "react";
import { useCopy } from "./hooks.js";

// A mark is one glyph carrying one fact: on, off, working, broken.
export function Mark({ kind = "on", title }) {
  const glyph = { on: "●", off: "○", sync: "◐", fail: "✗" }[kind] || "●";
  return (
    <span className={"mark is-" + kind} title={title} aria-label={title}>
      {glyph}
    </span>
  );
}

export function repoMarkKind(status) {
  if (status === "failed") return "fail";
  if (status === "syncing") return "sync";
  if (status === "pending" || !status) return "off";
  return "on";
}

export function Section({ title, count, right, children, id }) {
  return (
    <section className="section" id={id}>
      <header className="section-head">
        <h2>
          {title}
          {count !== undefined && count !== null ? <span className="n">{count}</span> : null}
        </h2>
        {right ? <div className="section-right">{right}</div> : null}
      </header>
      {children}
    </section>
  );
}

export function Empty({ children, hint }) {
  return (
    <div className="empty">
      <p>{children}</p>
      {hint ? <p className="empty-hint">{hint}</p> : null}
    </div>
  );
}

export function ErrorNote({ error, onRetry }) {
  if (!error) return null;
  return (
    <div className="notice is-err" role="alert">
      <span className="notice-mark">{"✗"}</span>
      <span className="notice-body">{String(error.message || error)}</span>
      {onRetry ? (
        <button type="button" className="btn" onClick={onRetry}>
          retry
        </button>
      ) : null}
    </div>
  );
}

// A chip is a filter you can see the size of. Clicking toggles it, so the
// count and the control are the same object.
export function Chip({ active, onClick, children, count, tone, title }) {
  return (
    <button
      type="button"
      className={"chip" + (active ? " is-active" : "") + (tone ? " is-" + tone : "")}
      onClick={onClick}
      title={title}
      aria-pressed={active}
    >
      <span className="chip-label">{children}</span>
      {count !== undefined && count !== null ? <span className="chip-n">{count}</span> : null}
    </button>
  );
}

// A segmented control for the two or three values a field can hold. Cheaper
// to read than a select, and it shows the alternatives without a click.
export function Segmented({ value, onChange, options, label }) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      {options.map((o) => {
        const v = typeof o === "string" ? o : o.value;
        const text = typeof o === "string" ? o : o.label;
        return (
          <button
            type="button"
            key={v}
            className={"seg" + (value === v ? " is-active" : "")}
            onClick={() => onChange(v)}
            aria-pressed={value === v}
          >
            {text}
          </button>
        );
      })}
    </div>
  );
}

export function Field({ label, value, onChange, placeholder, inputRef, wide }) {
  return (
    <label className={"field" + (wide ? " is-wide" : "")}>
      <span className="field-label">{label}</span>
      <span className="field-box">
        <input
          ref={inputRef}
          value={value}
          placeholder={placeholder}
          spellCheck="false"
          autoComplete="off"
          onChange={(e) => onChange(e.target.value)}
        />
        {value ? (
          <button
            type="button"
            className="field-clear"
            title={"clear " + label}
            onClick={() => onChange("")}
          >
            {"×"}
          </button>
        ) : null}
      </span>
    </label>
  );
}

// Kv is the detail grid. Rows with no value are dropped rather than shown
// empty: a page of blank labels tells you nothing about what is missing.
export function Kv({ rows }) {
  const kept = rows.filter(([, v]) => v !== null && v !== undefined && v !== "" && v !== false);
  if (kept.length === 0) return null;
  return (
    <dl className="kv">
      {kept.map(([k, v, cls]) => (
        <div className="kv-row" key={k}>
          <dt>{k}</dt>
          <dd className={cls}>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Copyable({ text, label, className }) {
  const [copied, copy] = useCopy();
  if (!text) return null;
  return (
    <button
      type="button"
      className={"copyable " + (className || "")}
      title="copy"
      onClick={() => copy(text, label || text)}
    >
      <span className="copyable-text">{text}</span>
      <span className="copyable-hint">{copied ? "copied" : "copy"}</span>
    </button>
  );
}

// Bar draws a duration against the slowest call in view. It is a background,
// not a chart -- the number is still the thing being read.
export function Bar({ value, max }) {
  const pct = max > 0 ? Math.min(100, Math.max(1, (value / max) * 100)) : 0;
  if (!value || !max) return null;
  return <span className="bar" style={{ width: pct + "%" }} aria-hidden="true" />;
}

// Pill is a status word with a colour: ready, failed, empty.
export function Pill({ tone, children, title }) {
  return (
    <span className={"pill" + (tone ? " is-" + tone : "")} title={title}>
      {children}
    </span>
  );
}

export function outcomeTone(outcome) {
  if (outcome === "error") return "err";
  if (outcome === "empty") return "warn";
  if (outcome === "cancelled") return "dim";
  return "ok";
}

// Code is a scrollable block for yaml and JSON. It never uses innerHTML: a
// repo name, a git error and a recorded argument all land here, and under
// `default-src 'self'` an injected script would be blocked, but a page that
// relies on the CSP to be safe is a page with a bug in it.
export function Code({ text, copy = true }) {
  if (!text) return null;
  return (
    <div className="code">
      {copy ? <CopyButton text={text} /> : null}
      <pre>{text}</pre>
    </div>
  );
}

export function CopyButton({ text, label = "copy" }) {
  const [copied, doCopy] = useCopy();
  return (
    <button type="button" className="btn code-copy" onClick={() => doCopy(text, label)}>
      {copied ? "copied" : label}
    </button>
  );
}

// Skeleton holds the height of a table that has not arrived, so the first
// paint does not shove the page down when it does.
export function Skeleton({ rows = 6 }) {
  return (
    <div className="skeleton" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <span key={i} className="skeleton-row" />
      ))}
    </div>
  );
}

// useAutoFocus puts the caret in the search box when "/" is pressed.
export function useSearchFocus() {
  const ref = useRef(null);
  useEffect(() => {
    const onKey = (e) => {
      if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target;
      if (t && /^(INPUT|SELECT|TEXTAREA)$/.test(t.tagName)) return;
      e.preventDefault();
      ref.current?.focus();
      ref.current?.select();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  return ref;
}
