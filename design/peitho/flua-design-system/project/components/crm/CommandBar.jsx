import React from "react";
import { Icon } from "../core/Icon.jsx";
import { Kbd } from "../core/Kbd.jsx";

/**
 * ⌘K command palette — the central practicality element of Peitho.
 * Controlled via `open` / `onClose`. Filters grouped commands and
 * supports arrow-key navigation + Enter.
 */
export function CommandBar({ open, onClose, groups = [], placeholder = "Buscar ou executar um comando…", style }) {
  const [query, setQuery] = React.useState("");
  const [cursor, setCursor] = React.useState(0);
  const inputRef = React.useRef(null);

  // Flatten + filter
  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase();
    return groups
      .map((g) => ({
        ...g,
        items: g.items.filter((it) => !q || (it.label + " " + (it.hint || "")).toLowerCase().includes(q)),
      }))
      .filter((g) => g.items.length > 0);
  }, [groups, query]);

  const flat = React.useMemo(() => filtered.flatMap((g) => g.items), [filtered]);

  React.useEffect(() => { if (open) { setQuery(""); setCursor(0); setTimeout(() => inputRef.current?.focus(), 20); } }, [open]);
  React.useEffect(() => { setCursor(0); }, [query]);

  const run = (it) => { it && it.onRun && it.onRun(); onClose && onClose(); };

  const onKey = (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); setCursor((c) => Math.min(c + 1, flat.length - 1)); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setCursor((c) => Math.max(c - 1, 0)); }
    else if (e.key === "Enter") { e.preventDefault(); run(flat[cursor]); }
    else if (e.key === "Escape") { onClose && onClose(); }
  };

  if (!open) return null;
  let idx = -1;

  return (
    <div
      onMouseDown={(e) => { if (e.target === e.currentTarget) onClose && onClose(); }}
      style={{
        position: "fixed", inset: 0, zIndex: 1000,
        display: "flex", alignItems: "flex-start", justifyContent: "center",
        padding: "12vh 16px 16px",
        background: "rgba(15,17,23,0.42)", backdropFilter: "blur(2px)", WebkitBackdropFilter: "blur(2px)",
      }}
    >
      <div
        onKeyDown={onKey}
        style={{
          width: "100%", maxWidth: 560, background: "var(--surface)",
          border: "1px solid var(--border)", borderRadius: "var(--radius-xl)",
          boxShadow: "var(--shadow-xl)", overflow: "hidden", ...style,
        }}
      >
        {/* Search row */}
        <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "0 14px",
          height: 52, borderBottom: "1px solid var(--border-subtle)" }}>
          <Icon name="search" size={18} style={{ color: "var(--text-tertiary)" }} />
          <input
            ref={inputRef} value={query} onChange={(e) => setQuery(e.target.value)} placeholder={placeholder}
            style={{ flex: 1, border: "none", outline: "none", background: "transparent",
              fontFamily: "var(--font-sans)", fontSize: "var(--text-lg)", color: "var(--text-primary)" }}
          />
          <Kbd>Esc</Kbd>
        </div>

        {/* Results */}
        <div style={{ maxHeight: 360, overflowY: "auto", padding: 6 }}>
          {flat.length === 0 && (
            <div style={{ padding: "28px 16px", textAlign: "center", fontSize: "var(--text-sm)", color: "var(--text-tertiary)" }}>
              Nenhum resultado para “{query}”
            </div>
          )}
          {filtered.map((g) => (
            <div key={g.label} style={{ marginBottom: 4 }}>
              {g.label && <div className="eyebrow" style={{ padding: "8px 10px 4px" }}>{g.label}</div>}
              {g.items.map((it) => {
                idx++;
                const sel = idx === cursor;
                const myIdx = idx;
                return (
                  <button
                    key={it.label}
                    onMouseMove={() => setCursor(myIdx)}
                    onClick={() => run(it)}
                    style={{
                      display: "flex", alignItems: "center", gap: 11, width: "100%",
                      padding: "9px 10px", border: "none", borderRadius: "var(--radius-md)",
                      cursor: "pointer", textAlign: "left",
                      background: sel ? "var(--accent-soft)" : "transparent",
                      color: sel ? "var(--accent)" : "var(--text-primary)",
                      transition: "background .1s ease",
                    }}
                  >
                    <span style={{ display: "inline-flex", color: sel ? "var(--accent)" : "var(--text-tertiary)" }}>
                      <Icon name={it.icon || "arrow-up-right"} size={17} />
                    </span>
                    <span style={{ flex: 1, fontSize: "var(--text-base)", fontWeight: "var(--weight-medium)" }}>{it.label}</span>
                    {it.hint && <span style={{ fontSize: "var(--text-xs)", color: "var(--text-tertiary)" }}>{it.hint}</span>}
                    {it.shortcut && (
                      <span style={{ display: "flex", gap: 3 }}>
                        {it.shortcut.map((k, i) => <Kbd key={i}>{k}</Kbd>)}
                      </span>
                    )}
                  </button>
                );
              })}
            </div>
          ))}
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: 14, padding: "8px 14px",
          borderTop: "1px solid var(--border-subtle)", fontSize: "var(--text-2xs)", color: "var(--text-tertiary)" }}>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><Kbd>↑</Kbd><Kbd>↓</Kbd> navegar</span>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 5 }}><Kbd>↵</Kbd> selecionar</span>
        </div>
      </div>
    </div>
  );
}
