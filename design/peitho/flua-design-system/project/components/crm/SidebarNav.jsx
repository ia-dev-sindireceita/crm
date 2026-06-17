import React from "react";
import { Icon } from "../core/Icon.jsx";
import { Tooltip } from "../surfaces/Tooltip.jsx";

/**
 * Collapsible CRM navigation sidebar.
 * Renders the Peitho mark, sections of nav items, and a collapse toggle.
 */
export function SidebarNav({
  items, sections, active, onSelect, collapsed = false, onToggle, footer, style,
}) {
  // Accept either flat `items` or grouped `sections`.
  const groups = sections || [{ items: items || [] }];

  return (
    <nav
      style={{
        display: "flex", flexDirection: "column",
        width: collapsed ? "var(--sidebar-collapsed)" : "var(--sidebar-w)",
        height: "100%", flexShrink: 0,
        background: "var(--bg-app)", borderRight: "1px solid var(--border)",
        transition: "width .18s cubic-bezier(.4,0,.2,1)", overflow: "hidden",
        ...style,
      }}
    >
      {/* Brand + collapse */}
      <div style={{ display: "flex", alignItems: "center", justifyContent: collapsed ? "center" : "space-between",
        height: "var(--topbar-h)", padding: collapsed ? 0 : "0 10px 0 14px", flexShrink: 0 }}>
        {!collapsed && (
          <div style={{ display: "flex", alignItems: "center", gap: 9 }}>
            <BrandMark />
            <span style={{ fontSize: 17, fontWeight: "var(--weight-semibold)", letterSpacing: "-0.02em",
              color: "var(--text-primary)" }}>Peitho</span>
          </div>
        )}
        {collapsed && <BrandMark />}
        {!collapsed && (
          <button onClick={onToggle} aria-label="Recolher menu"
            style={{ display: "inline-flex", padding: 6, border: "none", background: "transparent",
              color: "var(--text-tertiary)", cursor: "pointer", borderRadius: "var(--radius-sm)" }}>
            <Icon name="panel-left" size={17} />
          </button>
        )}
      </div>

      {/* Nav groups */}
      <div style={{ flex: 1, overflowY: "auto", padding: collapsed ? "4px 8px" : "4px 8px" }}>
        {groups.map((g, gi) => (
          <div key={gi} style={{ marginBottom: 10 }}>
            {g.label && !collapsed && (
              <div className="eyebrow" style={{ padding: "8px 8px 4px", color: "var(--text-tertiary)" }}>{g.label}</div>
            )}
            <div style={{ display: "flex", flexDirection: "column", gap: 1 }}>
              {g.items.map((it) => (
                <NavItem key={it.id} item={it} active={active === it.id} collapsed={collapsed} onSelect={onSelect} />
              ))}
            </div>
          </div>
        ))}
      </div>

      {footer && <div style={{ flexShrink: 0, padding: 8, borderTop: "1px solid var(--border-subtle)" }}>{footer}</div>}

      {collapsed && (
        <button onClick={onToggle} aria-label="Expandir menu"
          style={{ display: "flex", alignItems: "center", justifyContent: "center", height: 40,
            border: "none", borderTop: "1px solid var(--border-subtle)", background: "transparent",
            color: "var(--text-tertiary)", cursor: "pointer" }}>
          <Icon name="chevron-right" size={16} />
        </button>
      )}
    </nav>
  );
}

function NavItem({ item, active, collapsed, onSelect }) {
  const [hover, setHover] = React.useState(false);
  const body = (
    <button
      onClick={() => onSelect && onSelect(item.id)}
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: "flex", alignItems: "center", gap: 10, width: "100%",
        height: 34, padding: collapsed ? 0 : "0 10px", justifyContent: collapsed ? "center" : "flex-start",
        border: "none", borderRadius: "var(--radius-md)", cursor: "pointer",
        fontFamily: "var(--font-sans)", fontSize: "var(--text-base)",
        fontWeight: active ? "var(--weight-medium)" : "var(--weight-regular)",
        color: active ? "var(--accent)" : (hover ? "var(--text-primary)" : "var(--text-secondary)"),
        background: active ? "var(--accent-soft)" : (hover ? "var(--bg-subtle)" : "transparent"),
        transition: "background .12s ease, color .12s ease", textAlign: "left",
      }}
    >
      <Icon name={item.icon} size={17} />
      {!collapsed && <span style={{ flex: 1 }}>{item.label}</span>}
      {!collapsed && item.badge != null && (
        <span style={{ fontSize: "var(--text-2xs)", fontWeight: "var(--weight-semibold)",
          color: active ? "var(--accent)" : "var(--text-tertiary)",
          background: active ? "var(--surface)" : "var(--bg-subtle)",
          minWidth: 18, textAlign: "center", borderRadius: "var(--radius-full)", padding: "1px 6px" }}>{item.badge}</span>
      )}
    </button>
  );
  return collapsed ? <Tooltip label={item.label} side="right">{body}</Tooltip> : body;
}

function BrandMark() {
  return (
    <span style={{ display: "inline-flex", width: 26, height: 26, borderRadius: 7,
      background: "var(--accent)", alignItems: "center", justifyContent: "center", flexShrink: 0 }}>
      <svg width="26" height="26" viewBox="0 0 64 64" fill="none">
        <path d="M21 50 V14" stroke="#fff" strokeWidth="7" strokeLinecap="round" strokeLinejoin="round"/>
        <circle cx="32" cy="24" r="11" stroke="#fff" strokeWidth="7" fill="none"/>
      </svg>
    </span>
  );
}
