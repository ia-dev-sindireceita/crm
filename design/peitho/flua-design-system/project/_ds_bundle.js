/* @ds-bundle: {"format":3,"namespace":"FluaDesignSystem_2587b4","components":[{"name":"Button","sourcePath":"components/core/Button.jsx"},{"name":"ICON_PATHS","sourcePath":"components/core/Icon.jsx"},{"name":"Icon","sourcePath":"components/core/Icon.jsx"},{"name":"IconButton","sourcePath":"components/core/IconButton.jsx"},{"name":"Kbd","sourcePath":"components/core/Kbd.jsx"},{"name":"CommandBar","sourcePath":"components/crm/CommandBar.jsx"},{"name":"PIPELINE_STAGES","sourcePath":"components/crm/FunnelRow.jsx"},{"name":"FunnelRow","sourcePath":"components/crm/FunnelRow.jsx"},{"name":"LeadCard","sourcePath":"components/crm/LeadCard.jsx"},{"name":"SidebarNav","sourcePath":"components/crm/SidebarNav.jsx"},{"name":"Avatar","sourcePath":"components/data-display/Avatar.jsx"},{"name":"AvatarGroup","sourcePath":"components/data-display/AvatarGroup.jsx"},{"name":"Badge","sourcePath":"components/data-display/Badge.jsx"},{"name":"StatusBadge","sourcePath":"components/data-display/StatusBadge.jsx"},{"name":"Tag","sourcePath":"components/data-display/Tag.jsx"},{"name":"Checkbox","sourcePath":"components/forms/Checkbox.jsx"},{"name":"Input","sourcePath":"components/forms/Input.jsx"},{"name":"Select","sourcePath":"components/forms/Select.jsx"},{"name":"Switch","sourcePath":"components/forms/Switch.jsx"},{"name":"Card","sourcePath":"components/surfaces/Card.jsx"},{"name":"Tabs","sourcePath":"components/surfaces/Tabs.jsx"},{"name":"Tooltip","sourcePath":"components/surfaces/Tooltip.jsx"}],"sourceHashes":{"components/core/Button.jsx":"b3b6f578a33a","components/core/Icon.jsx":"b28c1435a4ea","components/core/IconButton.jsx":"e41bfc04a01b","components/core/Kbd.jsx":"c866984a527f","components/crm/CommandBar.jsx":"e6a17b4aa6fc","components/crm/FunnelRow.jsx":"3f9097be73a0","components/crm/LeadCard.jsx":"fb8149f32975","components/crm/SidebarNav.jsx":"c6fe04eaf463","components/data-display/Avatar.jsx":"e6496be2519d","components/data-display/AvatarGroup.jsx":"7f2b51cd59e2","components/data-display/Badge.jsx":"2347256490ab","components/data-display/StatusBadge.jsx":"edb9a3aedc1a","components/data-display/Tag.jsx":"7e9da5cc84cc","components/forms/Checkbox.jsx":"17d59ea01039","components/forms/Input.jsx":"a357616927d8","components/forms/Select.jsx":"a89021a5de0a","components/forms/Switch.jsx":"c4a751a7948d","components/surfaces/Card.jsx":"a3dff70927d0","components/surfaces/Tabs.jsx":"1c14350bd362","components/surfaces/Tooltip.jsx":"d3fa741e3c47","ui_kits/crm/AppShell.jsx":"edf1e45d7902","ui_kits/crm/ContactsView.jsx":"3beb9ad74db9","ui_kits/crm/DashboardView.jsx":"ba7bb997b16f","ui_kits/crm/InboxView.jsx":"e63b27c116b4","ui_kits/crm/MiscViews.jsx":"99b5e731b1fe","ui_kits/crm/PipelineView.jsx":"9e12eef59cf3","ui_kits/crm/data.js":"01fc0dc6ffd2"},"inlinedExternals":[],"unexposedExports":[]} */

(() => {

const __ds_ns = (window.FluaDesignSystem_2587b4 = window.FluaDesignSystem_2587b4 || {});

const __ds_scope = {};

(__ds_ns.__errors = __ds_ns.__errors || []);

// components/core/Icon.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Peitho icon set — a curated subset of Lucide (https://lucide.dev),
 * MIT licensed. 24×24 viewBox, 2px stroke, round caps/joins.
 * Stroke scales with size, so icons stay crisp from 14px to 48px.
 */
const ICON_PATHS = {
  search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  "chevron-right": '<path d="m9 18 6-6-6-6"/>',
  "chevron-left": '<path d="m15 18-6-6 6-6"/>',
  "chevron-down": '<path d="m6 9 6 6 6-6"/>',
  "chevrons-left": '<path d="m11 17-5-5 5-5"/><path d="m18 17-5-5 5-5"/>',
  "more-horizontal": '<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>',
  "more-vertical": '<circle cx="12" cy="12" r="1"/><circle cx="12" cy="5" r="1"/><circle cx="12" cy="19" r="1"/>',
  settings: '<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
  user: '<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',
  inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
  megaphone: '<path d="m3 11 18-5v12L3 14v-3z"/><path d="M11.6 16.8a3 3 0 1 1-5.8-1.6"/>',
  package: '<path d="M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z"/><path d="M3.3 7 12 12l8.7-5"/><path d="M12 22V12"/>',
  "credit-card": '<rect width="20" height="14" x="2" y="5" rx="2"/><path d="M2 10h20"/>',
  "bar-chart": '<path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>',
  "trending-up": '<path d="M16 7h6v6"/><path d="m22 7-8.5 8.5-5-5L2 17"/>',
  zap: '<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>',
  sparkles: '<path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/>',
  bell: '<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>',
  phone: '<path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/>',
  mail: '<rect width="20" height="16" x="2" y="4" rx="2"/><path d="m22 7-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 7"/>',
  "message-circle": '<path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"/>',
  calendar: '<path d="M8 2v4"/><path d="M16 2v4"/><rect width="18" height="18" x="3" y="4" rx="2"/><path d="M3 10h18"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
  "panel-left": '<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>',
  "layout-dashboard": '<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>',
  "git-branch": '<line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>',
  filter: '<polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>',
  edit: '<path d="M12 20h9"/><path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z"/>',
  building: '<rect width="16" height="20" x="4" y="2" rx="2"/><path d="M9 22v-4h6v4"/><path d="M8 6h.01"/><path d="M16 6h.01"/><path d="M12 6h.01"/><path d="M12 10h.01"/><path d="M12 14h.01"/><path d="M16 10h.01"/><path d="M16 14h.01"/><path d="M8 10h.01"/><path d="M8 14h.01"/>',
  "dollar-sign": '<line x1="12" x2="12" y1="2" y2="22"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/>',
  "arrow-up-right": '<path d="M7 7h10v10"/><path d="M7 17 17 7"/>',
  star: '<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>',
  circle: '<circle cx="12" cy="12" r="10"/>',
  "check-circle": '<path d="M21.801 10A10 10 0 1 1 17 3.335"/><path d="m9 11 3 3L22 4"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
  moon: '<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>'
};

/**
 * Stroke-based icon. Inherits color via `currentColor`.
 */
function Icon({
  name,
  size = 16,
  strokeWidth = 2,
  className,
  style,
  ...rest
}) {
  const inner = ICON_PATHS[name];
  if (!inner) {
    if (typeof console !== "undefined") console.warn(`[Peitho] Unknown icon: "${name}"`);
    return null;
  }
  const filled = name === "circle";
  return /*#__PURE__*/React.createElement("svg", _extends({
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: filled ? "currentColor" : "none",
    stroke: filled ? "none" : "currentColor",
    strokeWidth: strokeWidth,
    strokeLinecap: "round",
    strokeLinejoin: "round",
    className: className,
    style: {
      flexShrink: 0,
      display: "block",
      ...style
    },
    "aria-hidden": "true",
    dangerouslySetInnerHTML: {
      __html: inner
    }
  }, rest));
}
Object.assign(__ds_scope, { ICON_PATHS, Icon });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Icon.jsx", error: String((e && e.message) || e) }); }

// components/core/Button.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const SIZES = {
  sm: {
    height: "var(--control-sm)",
    padding: "0 10px",
    font: "var(--text-sm)",
    gap: "5px",
    icon: 14
  },
  md: {
    height: "var(--control-md)",
    padding: "0 14px",
    font: "var(--text-base)",
    gap: "6px",
    icon: 16
  },
  lg: {
    height: "var(--control-lg)",
    padding: "0 18px",
    font: "var(--text-md)",
    gap: "8px",
    icon: 18
  }
};
function variantStyle(variant) {
  switch (variant) {
    case "secondary":
      return {
        background: "var(--surface)",
        color: "var(--text-primary)",
        boxShadow: "inset 0 0 0 1px var(--border)",
        "--hover-bg": "var(--surface-2)",
        "--hover-shadow": "inset 0 0 0 1px var(--border-strong)"
      };
    case "ghost":
      return {
        background: "transparent",
        color: "var(--text-secondary)",
        "--hover-bg": "var(--bg-subtle)",
        "--hover-shadow": "none",
        "--hover-color": "var(--text-primary)"
      };
    case "danger":
      return {
        background: "var(--rose-500)",
        color: "#fff",
        "--hover-bg": "var(--rose-600)",
        "--hover-shadow": "none"
      };
    case "primary":
    default:
      return {
        background: "var(--accent)",
        color: "var(--text-on-accent)",
        "--hover-bg": "var(--accent-hover)",
        "--hover-shadow": "none"
      };
  }
}

/** Primary action control. Variants, sizes, optional leading/trailing icons. */
function Button({
  children,
  variant = "primary",
  size = "md",
  leftIcon,
  rightIcon,
  disabled = false,
  fullWidth = false,
  type = "button",
  style,
  ...rest
}) {
  const s = SIZES[size] || SIZES.md;
  const v = variantStyle(variant);
  const [hover, setHover] = React.useState(false);
  const [active, setActive] = React.useState(false);
  return /*#__PURE__*/React.createElement("button", _extends({
    type: type,
    disabled: disabled,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => {
      setHover(false);
      setActive(false);
    },
    onMouseDown: () => setActive(true),
    onMouseUp: () => setActive(false),
    style: {
      display: fullWidth ? "flex" : "inline-flex",
      width: fullWidth ? "100%" : undefined,
      alignItems: "center",
      justifyContent: "center",
      gap: s.gap,
      height: s.height,
      padding: s.padding,
      fontFamily: "var(--font-sans)",
      fontSize: s.font,
      fontWeight: "var(--weight-medium)",
      lineHeight: 1,
      whiteSpace: "nowrap",
      border: "none",
      borderRadius: "var(--radius-md)",
      cursor: disabled ? "not-allowed" : "pointer",
      opacity: disabled ? 0.5 : 1,
      background: hover && !disabled && v["--hover-bg"] ? v["--hover-bg"] : v.background,
      color: hover && !disabled && v["--hover-color"] ? v["--hover-color"] : v.color,
      boxShadow: hover && !disabled && v["--hover-shadow"] !== undefined ? v["--hover-shadow"] : v.boxShadow,
      transform: active && !disabled ? "translateY(0.5px) scale(0.985)" : "none",
      transition: "background .14s ease, box-shadow .14s ease, color .14s ease, transform .06s ease",
      ...style
    }
  }, rest), leftIcon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: leftIcon,
    size: s.icon
  }), children, rightIcon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: rightIcon,
    size: s.icon
  }));
}
Object.assign(__ds_scope, { Button });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Button.jsx", error: String((e && e.message) || e) }); }

// components/core/IconButton.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const SIZES = {
  sm: 28,
  md: 32,
  lg: 38
};
const ICON = {
  sm: 15,
  md: 16,
  lg: 18
};

/** Square icon-only button. Use for toolbar / row actions. */
function IconButton({
  icon,
  size = "md",
  variant = "ghost",
  label,
  disabled = false,
  active = false,
  style,
  ...rest
}) {
  const px = SIZES[size] || SIZES.md;
  const [hover, setHover] = React.useState(false);
  const base = {
    background: "transparent",
    color: "var(--text-secondary)",
    boxShadow: "none"
  };
  const styles = {
    ghost: {
      ...base,
      hoverBg: "var(--bg-subtle)",
      hoverColor: "var(--text-primary)"
    },
    solid: {
      background: "var(--accent)",
      color: "#fff",
      boxShadow: "none",
      hoverBg: "var(--accent-hover)",
      hoverColor: "#fff"
    },
    outline: {
      background: "var(--surface)",
      color: "var(--text-secondary)",
      boxShadow: "inset 0 0 0 1px var(--border)",
      hoverBg: "var(--surface-2)",
      hoverColor: "var(--text-primary)"
    }
  }[variant] || base;
  const isActive = active;
  return /*#__PURE__*/React.createElement("button", _extends({
    type: "button",
    disabled: disabled,
    "aria-label": label,
    title: label,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      width: px,
      height: px,
      padding: 0,
      border: "none",
      borderRadius: "var(--radius-md)",
      cursor: disabled ? "not-allowed" : "pointer",
      opacity: disabled ? 0.45 : 1,
      background: isActive ? "var(--accent-soft)" : hover ? styles.hoverBg : styles.background,
      color: isActive ? "var(--accent)" : hover ? styles.hoverColor : styles.color,
      boxShadow: styles.boxShadow,
      transition: "background .14s ease, color .14s ease",
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: ICON[size] || 16
  }));
}
Object.assign(__ds_scope, { IconButton });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/IconButton.jsx", error: String((e && e.message) || e) }); }

// components/core/Kbd.jsx
try { (() => {
/** Keyboard key cap — for shortcuts like ⌘K. */
function Kbd({
  children,
  style
}) {
  return /*#__PURE__*/React.createElement("kbd", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      minWidth: 18,
      height: 18,
      padding: "0 5px",
      fontFamily: "var(--font-sans)",
      fontSize: "var(--text-2xs)",
      fontWeight: "var(--weight-medium)",
      lineHeight: 1,
      color: "var(--text-secondary)",
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-xs)",
      boxShadow: "0 1px 0 var(--border)",
      ...style
    }
  }, children);
}
Object.assign(__ds_scope, { Kbd });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Kbd.jsx", error: String((e && e.message) || e) }); }

// components/crm/CommandBar.jsx
try { (() => {
/**
 * ⌘K command palette — the central practicality element of Peitho.
 * Controlled via `open` / `onClose`. Filters grouped commands and
 * supports arrow-key navigation + Enter.
 */
function CommandBar({
  open,
  onClose,
  groups = [],
  placeholder = "Buscar ou executar um comando…",
  style
}) {
  const [query, setQuery] = React.useState("");
  const [cursor, setCursor] = React.useState(0);
  const inputRef = React.useRef(null);

  // Flatten + filter
  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase();
    return groups.map(g => ({
      ...g,
      items: g.items.filter(it => !q || (it.label + " " + (it.hint || "")).toLowerCase().includes(q))
    })).filter(g => g.items.length > 0);
  }, [groups, query]);
  const flat = React.useMemo(() => filtered.flatMap(g => g.items), [filtered]);
  React.useEffect(() => {
    if (open) {
      setQuery("");
      setCursor(0);
      setTimeout(() => inputRef.current?.focus(), 20);
    }
  }, [open]);
  React.useEffect(() => {
    setCursor(0);
  }, [query]);
  const run = it => {
    it && it.onRun && it.onRun();
    onClose && onClose();
  };
  const onKey = e => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor(c => Math.min(c + 1, flat.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor(c => Math.max(c - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      run(flat[cursor]);
    } else if (e.key === "Escape") {
      onClose && onClose();
    }
  };
  if (!open) return null;
  let idx = -1;
  return /*#__PURE__*/React.createElement("div", {
    onMouseDown: e => {
      if (e.target === e.currentTarget) onClose && onClose();
    },
    style: {
      position: "fixed",
      inset: 0,
      zIndex: 1000,
      display: "flex",
      alignItems: "flex-start",
      justifyContent: "center",
      padding: "12vh 16px 16px",
      background: "rgba(15,17,23,0.42)",
      backdropFilter: "blur(2px)",
      WebkitBackdropFilter: "blur(2px)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    onKeyDown: onKey,
    style: {
      width: "100%",
      maxWidth: 560,
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-xl)",
      boxShadow: "var(--shadow-xl)",
      overflow: "hidden",
      ...style
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10,
      padding: "0 14px",
      height: 52,
      borderBottom: "1px solid var(--border-subtle)"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "search",
    size: 18,
    style: {
      color: "var(--text-tertiary)"
    }
  }), /*#__PURE__*/React.createElement("input", {
    ref: inputRef,
    value: query,
    onChange: e => setQuery(e.target.value),
    placeholder: placeholder,
    style: {
      flex: 1,
      border: "none",
      outline: "none",
      background: "transparent",
      fontFamily: "var(--font-sans)",
      fontSize: "var(--text-lg)",
      color: "var(--text-primary)"
    }
  }), /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "Esc")), /*#__PURE__*/React.createElement("div", {
    style: {
      maxHeight: 360,
      overflowY: "auto",
      padding: 6
    }
  }, flat.length === 0 && /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "28px 16px",
      textAlign: "center",
      fontSize: "var(--text-sm)",
      color: "var(--text-tertiary)"
    }
  }, "Nenhum resultado para \u201C", query, "\u201D"), filtered.map(g => /*#__PURE__*/React.createElement("div", {
    key: g.label,
    style: {
      marginBottom: 4
    }
  }, g.label && /*#__PURE__*/React.createElement("div", {
    className: "eyebrow",
    style: {
      padding: "8px 10px 4px"
    }
  }, g.label), g.items.map(it => {
    idx++;
    const sel = idx === cursor;
    const myIdx = idx;
    return /*#__PURE__*/React.createElement("button", {
      key: it.label,
      onMouseMove: () => setCursor(myIdx),
      onClick: () => run(it),
      style: {
        display: "flex",
        alignItems: "center",
        gap: 11,
        width: "100%",
        padding: "9px 10px",
        border: "none",
        borderRadius: "var(--radius-md)",
        cursor: "pointer",
        textAlign: "left",
        background: sel ? "var(--accent-soft)" : "transparent",
        color: sel ? "var(--accent)" : "var(--text-primary)",
        transition: "background .1s ease"
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        display: "inline-flex",
        color: sel ? "var(--accent)" : "var(--text-tertiary)"
      }
    }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: it.icon || "arrow-up-right",
      size: 17
    })), /*#__PURE__*/React.createElement("span", {
      style: {
        flex: 1,
        fontSize: "var(--text-base)",
        fontWeight: "var(--weight-medium)"
      }
    }, it.label), it.hint && /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: "var(--text-xs)",
        color: "var(--text-tertiary)"
      }
    }, it.hint), it.shortcut && /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 3
      }
    }, it.shortcut.map((k, i) => /*#__PURE__*/React.createElement(__ds_scope.Kbd, {
      key: i
    }, k))));
  })))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 14,
      padding: "8px 14px",
      borderTop: "1px solid var(--border-subtle)",
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)"
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 5
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u2191"), /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u2193"), " navegar"), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 5
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u21B5"), " selecionar"))));
}
Object.assign(__ds_scope, { CommandBar });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/crm/CommandBar.jsx", error: String((e && e.message) || e) }); }

// components/data-display/Avatar.jsx
try { (() => {
const PALETTE = ["var(--avatar-1)", "var(--avatar-2)", "var(--avatar-3)", "var(--avatar-4)", "var(--avatar-5)", "var(--avatar-6)"];
const SIZES = {
  xs: 20,
  sm: 24,
  md: 32,
  lg: 40
};
const FONTS = {
  xs: 9,
  sm: 10,
  md: 12,
  lg: 15
};
function initials(name = "") {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}
function hashColor(name = "") {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = h * 31 + name.charCodeAt(i) >>> 0;
  return PALETTE[h % PALETTE.length];
}

/** Round avatar — image when `src` given, otherwise hashed-color initials. */
function Avatar({
  name = "",
  src,
  size = "md",
  status,
  style
}) {
  const px = SIZES[size] || SIZES.md;
  const color = hashColor(name);
  return /*#__PURE__*/React.createElement("span", {
    style: {
      position: "relative",
      display: "inline-flex",
      flexShrink: 0,
      ...style
    }
  }, src ? /*#__PURE__*/React.createElement("img", {
    src: src,
    alt: name,
    style: {
      width: px,
      height: px,
      borderRadius: "50%",
      objectFit: "cover",
      boxShadow: "inset 0 0 0 1px rgba(0,0,0,.06)"
    }
  }) : /*#__PURE__*/React.createElement("span", {
    "aria-label": name,
    style: {
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      width: px,
      height: px,
      borderRadius: "50%",
      background: color,
      color: "#fff",
      fontSize: FONTS[size] || 12,
      fontWeight: "var(--weight-semibold)",
      letterSpacing: "0.01em",
      userSelect: "none"
    }
  }, initials(name)), status && /*#__PURE__*/React.createElement("span", {
    style: {
      position: "absolute",
      right: -1,
      bottom: -1,
      width: Math.max(8, px * 0.28),
      height: Math.max(8, px * 0.28),
      borderRadius: "50%",
      border: "2px solid var(--surface)",
      background: status === "online" ? "var(--green-500)" : status === "busy" ? "var(--rose-500)" : "var(--gray-400)"
    }
  }));
}
Object.assign(__ds_scope, { Avatar });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data-display/Avatar.jsx", error: String((e && e.message) || e) }); }

// components/crm/FunnelRow.jsx
try { (() => {
/** Ordered pipeline stages with their dot color. */
const PIPELINE_STAGES = [{
  id: "new",
  label: "Novo",
  color: "var(--blue-500)"
}, {
  id: "qualified",
  label: "Qualificado",
  color: "var(--teal-500)"
}, {
  id: "negotiating",
  label: "Negociação",
  color: "var(--amber-500)"
}, {
  id: "won",
  label: "Fechado",
  color: "var(--green-500)"
}];

/**
 * Funnel table row with a segmented stage indicator.
 * Designed to sit inside a table-like flex column with a header row.
 */
function FunnelRow({
  deal,
  stages = PIPELINE_STAGES,
  onClick,
  style
}) {
  const {
    name,
    company,
    value,
    owner,
    stageIndex = 0,
    status
  } = deal || {};
  const [hover, setHover] = React.useState(false);
  const activeColor = stages[Math.min(stageIndex, stages.length - 1)]?.color || "var(--accent)";
  return /*#__PURE__*/React.createElement("div", {
    onClick: onClick,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      display: "grid",
      gridTemplateColumns: "1.6fr 1.4fr 0.9fr 36px",
      alignItems: "center",
      gap: 16,
      padding: "11px 16px",
      background: hover ? "var(--surface-2)" : "transparent",
      borderBottom: "1px solid var(--border-subtle)",
      cursor: "pointer",
      transition: "background .12s ease",
      ...style
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-base)",
      fontWeight: "var(--weight-medium)",
      color: "var(--text-primary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, name), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-xs)",
      color: "var(--text-tertiary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, company)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 3,
      marginBottom: 5
    }
  }, stages.map((st, i) => /*#__PURE__*/React.createElement("span", {
    key: st.id,
    style: {
      flex: 1,
      height: 4,
      borderRadius: "var(--radius-full)",
      background: i <= stageIndex ? activeColor : "var(--border)",
      transition: "background .2s ease"
    }
  }))), /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-2xs)",
      fontWeight: "var(--weight-medium)",
      color: activeColor
    }
  }, stages[Math.min(stageIndex, stages.length - 1)]?.label)), /*#__PURE__*/React.createElement("div", {
    className: "tnum",
    style: {
      fontSize: "var(--text-base)",
      fontWeight: "var(--weight-semibold)",
      color: "var(--text-primary)",
      textAlign: "right"
    }
  }, value), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      justifyContent: "center"
    }
  }, owner && /*#__PURE__*/React.createElement(__ds_scope.Avatar, {
    name: owner,
    size: "sm"
  })));
}
Object.assign(__ds_scope, { PIPELINE_STAGES, FunnelRow });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/crm/FunnelRow.jsx", error: String((e && e.message) || e) }); }

// components/data-display/AvatarGroup.jsx
try { (() => {
const SIZES = {
  xs: 20,
  sm: 24,
  md: 32,
  lg: 40
};

/** Overlapping stack of avatars with a +N overflow chip. */
function AvatarGroup({
  people = [],
  size = "sm",
  max = 4,
  style
}) {
  const px = SIZES[size] || SIZES.sm;
  const shown = people.slice(0, max);
  const extra = people.length - shown.length;
  const overlap = Math.round(px * 0.32);
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      ...style
    }
  }, shown.map((p, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    style: {
      marginLeft: i === 0 ? 0 : -overlap,
      borderRadius: "50%",
      boxShadow: "0 0 0 2px var(--surface)",
      position: "relative",
      zIndex: i
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Avatar, {
    name: p.name,
    src: p.src,
    size: size
  }))), extra > 0 && /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: -overlap,
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      width: px,
      height: px,
      borderRadius: "50%",
      background: "var(--bg-subtle)",
      color: "var(--text-secondary)",
      fontSize: size === "lg" ? 13 : 10,
      fontWeight: "var(--weight-semibold)",
      boxShadow: "0 0 0 2px var(--surface)",
      position: "relative",
      zIndex: shown.length
    }
  }, "+", extra));
}
Object.assign(__ds_scope, { AvatarGroup });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data-display/AvatarGroup.jsx", error: String((e && e.message) || e) }); }

// components/data-display/Badge.jsx
try { (() => {
const TONES = {
  neutral: {
    fg: "var(--status-neutral-fg)",
    bg: "var(--status-neutral-bg)"
  },
  accent: {
    fg: "var(--accent)",
    bg: "var(--accent-soft)"
  },
  won: {
    fg: "var(--status-won-fg)",
    bg: "var(--status-won-bg)"
  },
  lost: {
    fg: "var(--status-lost-fg)",
    bg: "var(--status-lost-bg)"
  },
  nego: {
    fg: "var(--status-nego-fg)",
    bg: "var(--status-nego-bg)"
  },
  info: {
    fg: "var(--status-info-fg)",
    bg: "var(--status-info-bg)"
  }
};

/** Small status pill. Soft tinted background, no border. */
function Badge({
  children,
  tone = "neutral",
  icon,
  dot = false,
  size = "md",
  style
}) {
  const t = TONES[tone] || TONES.neutral;
  const sm = size === "sm";
  return /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: sm ? 4 : 5,
      height: sm ? 18 : 20,
      padding: sm ? "0 6px" : "0 8px",
      fontSize: sm ? "var(--text-2xs)" : "var(--text-xs)",
      fontWeight: "var(--weight-medium)",
      lineHeight: 1,
      whiteSpace: "nowrap",
      color: t.fg,
      background: t.bg,
      borderRadius: "var(--radius-full)",
      ...style
    }
  }, dot && /*#__PURE__*/React.createElement("span", {
    style: {
      width: 6,
      height: 6,
      borderRadius: "50%",
      background: t.fg,
      flexShrink: 0
    }
  }), icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: sm ? 11 : 12
  }), children);
}
Object.assign(__ds_scope, { Badge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data-display/Badge.jsx", error: String((e && e.message) || e) }); }

// components/data-display/StatusBadge.jsx
try { (() => {
/** Maps CRM deal stages to a Badge tone + label. The canonical won/lost/negotiating indicator. */
const STAGE = {
  won: {
    tone: "won",
    label: "Ganho",
    dot: true
  },
  lost: {
    tone: "lost",
    label: "Perdido",
    dot: true
  },
  negotiating: {
    tone: "nego",
    label: "Em negociação",
    dot: true
  },
  new: {
    tone: "info",
    label: "Novo",
    dot: true
  },
  qualified: {
    tone: "accent",
    label: "Qualificado",
    dot: true
  },
  open: {
    tone: "neutral",
    label: "Aberto",
    dot: true
  }
};
function StatusBadge({
  status = "open",
  label,
  size = "md",
  style
}) {
  const cfg = STAGE[status] || STAGE.open;
  return /*#__PURE__*/React.createElement(__ds_scope.Badge, {
    tone: cfg.tone,
    dot: cfg.dot,
    size: size,
    style: style
  }, label || cfg.label);
}
Object.assign(__ds_scope, { StatusBadge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data-display/StatusBadge.jsx", error: String((e && e.message) || e) }); }

// components/crm/LeadCard.jsx
try { (() => {
/** Contact / lead card: avatar, name, company, status, value, quick actions. */
function LeadCard({
  lead,
  onAction,
  style
}) {
  const {
    name,
    company,
    status,
    value,
    owner,
    email,
    lastActivity,
    tags
  } = lead || {};
  const [hover, setHover] = React.useState(false);
  return /*#__PURE__*/React.createElement("div", {
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderColor: hover ? "var(--border-strong)" : "var(--border)",
      borderRadius: "var(--radius-lg)",
      padding: 14,
      boxShadow: hover ? "var(--shadow-md)" : "var(--shadow-sm)",
      transition: "box-shadow .16s ease, border-color .16s ease",
      ...style
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "flex-start",
      gap: 11
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Avatar, {
    name: name,
    size: "md"
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 8
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: "var(--weight-semibold)",
      color: "var(--text-primary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, name), status && /*#__PURE__*/React.createElement(__ds_scope.StatusBadge, {
    status: status,
    size: "sm"
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 5,
      marginTop: 2,
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "building",
    size: 13,
    style: {
      color: "var(--text-tertiary)"
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, company))), value != null && /*#__PURE__*/React.createElement("div", {
    style: {
      textAlign: "right",
      flexShrink: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "tnum",
    style: {
      fontSize: "var(--text-md)",
      fontWeight: "var(--weight-semibold)",
      color: "var(--text-primary)"
    }
  }, value), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)"
    }
  }, "valor"))), tags && tags.length > 0 && /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      flexWrap: "wrap",
      gap: 5,
      marginTop: 11
    }
  }, tags.map(t => /*#__PURE__*/React.createElement("span", {
    key: t,
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-secondary)",
      background: "var(--bg-subtle)",
      padding: "2px 7px",
      borderRadius: "var(--radius-sm)"
    }
  }, t))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginTop: 12,
      paddingTop: 11,
      borderTop: "1px solid var(--border-subtle)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 6,
      minWidth: 0
    }
  }, owner && /*#__PURE__*/React.createElement(__ds_scope.Avatar, {
    name: owner,
    size: "xs"
  }), lastActivity && /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 4,
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "clock",
    size: 11
  }), lastActivity)), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 2,
      opacity: hover ? 1 : 0.55,
      transition: "opacity .14s ease"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "phone",
    size: "sm",
    label: "Ligar",
    onClick: () => onAction && onAction("call", lead)
  }), /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "mail",
    size: "sm",
    label: "E-mail",
    onClick: () => onAction && onAction("mail", lead)
  }), /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "message-circle",
    size: "sm",
    label: "Mensagem",
    onClick: () => onAction && onAction("message", lead)
  }))));
}
Object.assign(__ds_scope, { LeadCard });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/crm/LeadCard.jsx", error: String((e && e.message) || e) }); }

// components/data-display/Tag.jsx
try { (() => {
/** Removable tag / label chip. Neutral by default. */
function Tag({
  children,
  onRemove,
  icon,
  color,
  style
}) {
  return /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 5,
      height: 22,
      padding: onRemove ? "0 5px 0 8px" : "0 9px",
      fontSize: "var(--text-xs)",
      fontWeight: "var(--weight-medium)",
      color: "var(--text-secondary)",
      background: "var(--bg-subtle)",
      border: "1px solid var(--border-subtle)",
      borderRadius: "var(--radius-sm)",
      lineHeight: 1,
      ...style
    }
  }, color && /*#__PURE__*/React.createElement("span", {
    style: {
      width: 7,
      height: 7,
      borderRadius: "50%",
      background: color,
      flexShrink: 0
    }
  }), icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: 12
  }), children, onRemove && /*#__PURE__*/React.createElement("button", {
    type: "button",
    onClick: onRemove,
    "aria-label": "Remover",
    style: {
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      width: 16,
      height: 16,
      marginLeft: 1,
      padding: 0,
      border: "none",
      background: "transparent",
      color: "var(--text-tertiary)",
      borderRadius: "var(--radius-xs)",
      cursor: "pointer"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "x",
    size: 11
  })));
}
Object.assign(__ds_scope, { Tag });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data-display/Tag.jsx", error: String((e && e.message) || e) }); }

// components/forms/Checkbox.jsx
try { (() => {
/** Checkbox with accent fill + check icon when selected. */
function Checkbox({
  checked,
  defaultChecked,
  onChange,
  disabled,
  label,
  indeterminate,
  style
}) {
  const [internal, setInternal] = React.useState(!!defaultChecked);
  const isOn = checked !== undefined ? checked : internal;
  const toggle = () => {
    if (disabled) return;
    if (checked === undefined) setInternal(!isOn);
    onChange && onChange(!isOn);
  };
  const box = /*#__PURE__*/React.createElement("span", {
    onClick: toggle,
    role: "checkbox",
    "aria-checked": indeterminate ? "mixed" : isOn,
    style: {
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      width: 17,
      height: 17,
      flexShrink: 0,
      borderRadius: "var(--radius-xs)",
      cursor: disabled ? "not-allowed" : "pointer",
      opacity: disabled ? 0.5 : 1,
      background: isOn || indeterminate ? "var(--accent)" : "var(--surface)",
      boxShadow: isOn || indeterminate ? "none" : "inset 0 0 0 1.5px var(--border-strong)",
      color: "#fff",
      transition: "background .12s ease, box-shadow .12s ease"
    }
  }, indeterminate ? /*#__PURE__*/React.createElement("span", {
    style: {
      width: 8,
      height: 2,
      borderRadius: 1,
      background: "#fff"
    }
  }) : isOn && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "check",
    size: 12,
    strokeWidth: 3
  }));
  if (!label) return /*#__PURE__*/React.createElement("span", {
    style: style
  }, box);
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 8,
      cursor: disabled ? "not-allowed" : "pointer",
      ...style
    }
  }, box, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-base)",
      color: "var(--text-primary)"
    }
  }, label));
}
Object.assign(__ds_scope, { Checkbox });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Checkbox.jsx", error: String((e && e.message) || e) }); }

// components/forms/Input.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const SIZES = {
  sm: {
    h: "var(--control-sm)",
    font: "var(--text-sm)",
    pad: 10
  },
  md: {
    h: "var(--control-md)",
    font: "var(--text-base)",
    pad: 12
  },
  lg: {
    h: "var(--control-lg)",
    font: "var(--text-md)",
    pad: 14
  }
};

/** Text input with optional leading icon and inline label. */
function Input({
  size = "md",
  leftIcon,
  label,
  hint,
  error,
  value,
  defaultValue,
  placeholder,
  type = "text",
  disabled,
  style,
  containerStyle,
  ...rest
}) {
  const s = SIZES[size] || SIZES.md;
  const [focus, setFocus] = React.useState(false);
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: "block",
      ...containerStyle
    }
  }, label && /*#__PURE__*/React.createElement("span", {
    style: {
      display: "block",
      marginBottom: 6,
      fontSize: "var(--text-xs)",
      fontWeight: "var(--weight-medium)",
      color: "var(--text-secondary)"
    }
  }, label), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 7,
      height: s.h,
      padding: `0 ${s.pad}px`,
      background: disabled ? "var(--bg-subtle)" : "var(--surface)",
      borderRadius: "var(--radius-md)",
      boxShadow: error ? "inset 0 0 0 1px var(--rose-500)" : focus ? "inset 0 0 0 1px var(--accent), 0 0 0 3px var(--accent-soft)" : "inset 0 0 0 1px var(--border)",
      transition: "box-shadow .14s ease"
    }
  }, leftIcon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: leftIcon,
    size: s === SIZES.sm ? 14 : 16,
    style: {
      color: "var(--text-tertiary)"
    }
  }), /*#__PURE__*/React.createElement("input", _extends({
    type: type,
    value: value,
    defaultValue: defaultValue,
    placeholder: placeholder,
    disabled: disabled,
    onFocus: () => setFocus(true),
    onBlur: () => setFocus(false),
    style: {
      flex: 1,
      minWidth: 0,
      height: "100%",
      border: "none",
      outline: "none",
      background: "transparent",
      color: "var(--text-primary)",
      fontFamily: "var(--font-sans)",
      fontSize: s.font,
      padding: 0,
      ...style
    }
  }, rest))), (hint || error) && /*#__PURE__*/React.createElement("span", {
    style: {
      display: "block",
      marginTop: 5,
      fontSize: "var(--text-xs)",
      color: error ? "var(--rose-500)" : "var(--text-tertiary)"
    }
  }, error || hint));
}
Object.assign(__ds_scope, { Input });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Input.jsx", error: String((e && e.message) || e) }); }

// components/forms/Select.jsx
try { (() => {
const SIZES = {
  sm: {
    h: "var(--control-sm)",
    font: "var(--text-sm)"
  },
  md: {
    h: "var(--control-md)",
    font: "var(--text-base)"
  }
};

/** Native select wrapped to match Peitho inputs. */
function Select({
  size = "md",
  label,
  value,
  defaultValue,
  onChange,
  options = [],
  disabled,
  style,
  containerStyle
}) {
  const s = SIZES[size] || SIZES.md;
  const [focus, setFocus] = React.useState(false);
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: "block",
      ...containerStyle
    }
  }, label && /*#__PURE__*/React.createElement("span", {
    style: {
      display: "block",
      marginBottom: 6,
      fontSize: "var(--text-xs)",
      fontWeight: "var(--weight-medium)",
      color: "var(--text-secondary)"
    }
  }, label), /*#__PURE__*/React.createElement("span", {
    style: {
      position: "relative",
      display: "block"
    }
  }, /*#__PURE__*/React.createElement("select", {
    value: value,
    defaultValue: defaultValue,
    disabled: disabled,
    onChange: e => onChange && onChange(e.target.value),
    onFocus: () => setFocus(true),
    onBlur: () => setFocus(false),
    style: {
      appearance: "none",
      WebkitAppearance: "none",
      width: "100%",
      height: s.h,
      padding: "0 32px 0 12px",
      fontFamily: "var(--font-sans)",
      fontSize: s.font,
      color: "var(--text-primary)",
      background: disabled ? "var(--bg-subtle)" : "var(--surface)",
      border: "none",
      borderRadius: "var(--radius-md)",
      cursor: disabled ? "not-allowed" : "pointer",
      boxShadow: focus ? "inset 0 0 0 1px var(--accent), 0 0 0 3px var(--accent-soft)" : "inset 0 0 0 1px var(--border)",
      transition: "box-shadow .14s ease",
      outline: "none",
      ...style
    }
  }, options.map(o => {
    const opt = typeof o === "string" ? {
      value: o,
      label: o
    } : o;
    return /*#__PURE__*/React.createElement("option", {
      key: opt.value,
      value: opt.value
    }, opt.label);
  })), /*#__PURE__*/React.createElement("span", {
    style: {
      position: "absolute",
      right: 10,
      top: "50%",
      transform: "translateY(-50%)",
      pointerEvents: "none",
      color: "var(--text-tertiary)",
      display: "flex"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-down",
    size: 15
  }))));
}
Object.assign(__ds_scope, { Select });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Select.jsx", error: String((e && e.message) || e) }); }

// components/forms/Switch.jsx
try { (() => {
/** Toggle switch — compact, accent-filled when on. */
function Switch({
  checked,
  defaultChecked,
  onChange,
  disabled,
  label,
  style
}) {
  const [internal, setInternal] = React.useState(!!defaultChecked);
  const isOn = checked !== undefined ? checked : internal;
  const toggle = () => {
    if (disabled) return;
    if (checked === undefined) setInternal(!isOn);
    onChange && onChange(!isOn);
  };
  const sw = /*#__PURE__*/React.createElement("button", {
    type: "button",
    role: "switch",
    "aria-checked": isOn,
    disabled: disabled,
    onClick: toggle,
    style: {
      position: "relative",
      width: 34,
      height: 20,
      flexShrink: 0,
      padding: 0,
      border: "none",
      borderRadius: "var(--radius-full)",
      cursor: disabled ? "not-allowed" : "pointer",
      background: isOn ? "var(--accent)" : "var(--gray-300)",
      opacity: disabled ? 0.5 : 1,
      transition: "background .16s ease"
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      position: "absolute",
      top: 2,
      left: isOn ? 16 : 2,
      width: 16,
      height: 16,
      borderRadius: "50%",
      background: "#fff",
      boxShadow: "0 1px 2px rgba(0,0,0,.25)",
      transition: "left .16s cubic-bezier(.4,0,.2,1)"
    }
  }));
  if (!label) return /*#__PURE__*/React.createElement("span", {
    style: style
  }, sw);
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 9,
      cursor: disabled ? "not-allowed" : "pointer",
      ...style
    }
  }, sw, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-base)",
      color: "var(--text-primary)"
    }
  }, label));
}
Object.assign(__ds_scope, { Switch });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Switch.jsx", error: String((e && e.message) || e) }); }

// components/surfaces/Card.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/** Surface container. Border + soft shadow, no heavy elevation. */
function Card({
  children,
  padding = 16,
  interactive = false,
  style,
  ...rest
}) {
  const [hover, setHover] = React.useState(false);
  return /*#__PURE__*/React.createElement("div", _extends({
    onMouseEnter: () => interactive && setHover(true),
    onMouseLeave: () => interactive && setHover(false),
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      boxShadow: hover ? "var(--shadow-md)" : "var(--shadow-sm)",
      padding,
      cursor: interactive ? "pointer" : "default",
      transition: "box-shadow .16s ease, border-color .16s ease, transform .12s ease",
      borderColor: hover ? "var(--border-strong)" : "var(--border)",
      ...style
    }
  }, rest), children);
}
Object.assign(__ds_scope, { Card });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/surfaces/Card.jsx", error: String((e && e.message) || e) }); }

// components/surfaces/Tabs.jsx
try { (() => {
/** Underline tab bar. Controlled via `value` or uncontrolled via `defaultValue`. */
function Tabs({
  items = [],
  value,
  defaultValue,
  onChange,
  style
}) {
  const [internal, setInternal] = React.useState(defaultValue ?? (items[0] && items[0].value));
  const active = value !== undefined ? value : internal;
  const select = v => {
    if (value === undefined) setInternal(v);
    onChange && onChange(v);
  };
  return /*#__PURE__*/React.createElement("div", {
    role: "tablist",
    style: {
      display: "flex",
      gap: 2,
      borderBottom: "1px solid var(--border)",
      ...style
    }
  }, items.map(it => {
    const on = it.value === active;
    return /*#__PURE__*/React.createElement("button", {
      key: it.value,
      role: "tab",
      "aria-selected": on,
      onClick: () => select(it.value),
      style: {
        position: "relative",
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        height: 34,
        padding: "0 12px",
        border: "none",
        background: "transparent",
        fontFamily: "var(--font-sans)",
        fontSize: "var(--text-sm)",
        fontWeight: "var(--weight-medium)",
        cursor: "pointer",
        color: on ? "var(--text-primary)" : "var(--text-secondary)",
        transition: "color .14s ease"
      }
    }, it.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: it.icon,
      size: 15
    }), it.label, it.count != null && /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: "var(--text-2xs)",
        color: "var(--text-tertiary)",
        background: "var(--bg-subtle)",
        borderRadius: "var(--radius-full)",
        padding: "1px 6px"
      }
    }, it.count), /*#__PURE__*/React.createElement("span", {
      style: {
        position: "absolute",
        left: 6,
        right: 6,
        bottom: -1,
        height: 2,
        borderRadius: "2px 2px 0 0",
        background: on ? "var(--accent)" : "transparent",
        transition: "background .14s ease"
      }
    }));
  }));
}
Object.assign(__ds_scope, { Tabs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/surfaces/Tabs.jsx", error: String((e && e.message) || e) }); }

// components/surfaces/Tooltip.jsx
try { (() => {
/** Lightweight hover tooltip. Wraps a single trigger child. */
function Tooltip({
  label,
  side = "top",
  children,
  style
}) {
  const [show, setShow] = React.useState(false);
  const pos = {
    top: {
      bottom: "calc(100% + 6px)",
      left: "50%",
      transform: "translateX(-50%)"
    },
    bottom: {
      top: "calc(100% + 6px)",
      left: "50%",
      transform: "translateX(-50%)"
    },
    left: {
      right: "calc(100% + 6px)",
      top: "50%",
      transform: "translateY(-50%)"
    },
    right: {
      left: "calc(100% + 6px)",
      top: "50%",
      transform: "translateY(-50%)"
    }
  }[side];
  return /*#__PURE__*/React.createElement("span", {
    style: {
      position: "relative",
      display: "inline-flex",
      ...style
    },
    onMouseEnter: () => setShow(true),
    onMouseLeave: () => setShow(false)
  }, children, show && /*#__PURE__*/React.createElement("span", {
    role: "tooltip",
    style: {
      position: "absolute",
      zIndex: 50,
      ...pos,
      whiteSpace: "nowrap",
      padding: "5px 8px",
      fontSize: "var(--text-xs)",
      fontWeight: "var(--weight-medium)",
      color: "var(--gray-25)",
      background: "var(--gray-800)",
      borderRadius: "var(--radius-sm)",
      boxShadow: "var(--shadow-md)",
      pointerEvents: "none"
    }
  }, label));
}
Object.assign(__ds_scope, { Tooltip });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/surfaces/Tooltip.jsx", error: String((e && e.message) || e) }); }

// components/crm/SidebarNav.jsx
try { (() => {
/**
 * Collapsible CRM navigation sidebar.
 * Renders the Peitho mark, sections of nav items, and a collapse toggle.
 */
function SidebarNav({
  items,
  sections,
  active,
  onSelect,
  collapsed = false,
  onToggle,
  footer,
  style
}) {
  // Accept either flat `items` or grouped `sections`.
  const groups = sections || [{
    items: items || []
  }];
  return /*#__PURE__*/React.createElement("nav", {
    style: {
      display: "flex",
      flexDirection: "column",
      width: collapsed ? "var(--sidebar-collapsed)" : "var(--sidebar-w)",
      height: "100%",
      flexShrink: 0,
      background: "var(--bg-app)",
      borderRight: "1px solid var(--border)",
      transition: "width .18s cubic-bezier(.4,0,.2,1)",
      overflow: "hidden",
      ...style
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: collapsed ? "center" : "space-between",
      height: "var(--topbar-h)",
      padding: collapsed ? 0 : "0 10px 0 14px",
      flexShrink: 0
    }
  }, !collapsed && /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 9
    }
  }, /*#__PURE__*/React.createElement(BrandMark, null), /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 17,
      fontWeight: "var(--weight-semibold)",
      letterSpacing: "-0.02em",
      color: "var(--text-primary)"
    }
  }, "Peitho")), collapsed && /*#__PURE__*/React.createElement(BrandMark, null), !collapsed && /*#__PURE__*/React.createElement("button", {
    onClick: onToggle,
    "aria-label": "Recolher menu",
    style: {
      display: "inline-flex",
      padding: 6,
      border: "none",
      background: "transparent",
      color: "var(--text-tertiary)",
      cursor: "pointer",
      borderRadius: "var(--radius-sm)"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "panel-left",
    size: 17
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      overflowY: "auto",
      padding: collapsed ? "4px 8px" : "4px 8px"
    }
  }, groups.map((g, gi) => /*#__PURE__*/React.createElement("div", {
    key: gi,
    style: {
      marginBottom: 10
    }
  }, g.label && !collapsed && /*#__PURE__*/React.createElement("div", {
    className: "eyebrow",
    style: {
      padding: "8px 8px 4px",
      color: "var(--text-tertiary)"
    }
  }, g.label), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      flexDirection: "column",
      gap: 1
    }
  }, g.items.map(it => /*#__PURE__*/React.createElement(NavItem, {
    key: it.id,
    item: it,
    active: active === it.id,
    collapsed: collapsed,
    onSelect: onSelect
  })))))), footer && /*#__PURE__*/React.createElement("div", {
    style: {
      flexShrink: 0,
      padding: 8,
      borderTop: "1px solid var(--border-subtle)"
    }
  }, footer), collapsed && /*#__PURE__*/React.createElement("button", {
    onClick: onToggle,
    "aria-label": "Expandir menu",
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "center",
      height: 40,
      border: "none",
      borderTop: "1px solid var(--border-subtle)",
      background: "transparent",
      color: "var(--text-tertiary)",
      cursor: "pointer"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-right",
    size: 16
  })));
}
function NavItem({
  item,
  active,
  collapsed,
  onSelect
}) {
  const [hover, setHover] = React.useState(false);
  const body = /*#__PURE__*/React.createElement("button", {
    onClick: () => onSelect && onSelect(item.id),
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10,
      width: "100%",
      height: 34,
      padding: collapsed ? 0 : "0 10px",
      justifyContent: collapsed ? "center" : "flex-start",
      border: "none",
      borderRadius: "var(--radius-md)",
      cursor: "pointer",
      fontFamily: "var(--font-sans)",
      fontSize: "var(--text-base)",
      fontWeight: active ? "var(--weight-medium)" : "var(--weight-regular)",
      color: active ? "var(--accent)" : hover ? "var(--text-primary)" : "var(--text-secondary)",
      background: active ? "var(--accent-soft)" : hover ? "var(--bg-subtle)" : "transparent",
      transition: "background .12s ease, color .12s ease",
      textAlign: "left"
    }
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: item.icon,
    size: 17
  }), !collapsed && /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, item.label), !collapsed && item.badge != null && /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-2xs)",
      fontWeight: "var(--weight-semibold)",
      color: active ? "var(--accent)" : "var(--text-tertiary)",
      background: active ? "var(--surface)" : "var(--bg-subtle)",
      minWidth: 18,
      textAlign: "center",
      borderRadius: "var(--radius-full)",
      padding: "1px 6px"
    }
  }, item.badge));
  return collapsed ? /*#__PURE__*/React.createElement(__ds_scope.Tooltip, {
    label: item.label,
    side: "right"
  }, body) : body;
}
function BrandMark() {
  return /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      width: 26,
      height: 26,
      borderRadius: 7,
      background: "var(--accent)",
      alignItems: "center",
      justifyContent: "center",
      flexShrink: 0
    }
  }, /*#__PURE__*/React.createElement("svg", {
    width: "26",
    height: "26",
    viewBox: "0 0 64 64",
    fill: "none"
  }, /*#__PURE__*/React.createElement("path", {
    d: "M21 50 V14",
    stroke: "#fff",
    strokeWidth: "7",
    strokeLinecap: "round",
    strokeLinejoin: "round"
  }), /*#__PURE__*/React.createElement("circle", {
    cx: "32",
    cy: "24",
    r: "11",
    stroke: "#fff",
    strokeWidth: "7",
    fill: "none"
  })));
}
Object.assign(__ds_scope, { SidebarNav });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/crm/SidebarNav.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/AppShell.jsx
try { (() => {
// AppShell — sidebar + topbar + content + ⌘K. Composes the views. Registers on window.
const FluaAS = window.FluaDesignSystem_2587b4;
const VIEW_TITLES = {
  dashboard: "Visão geral",
  pipeline: "Funil de vendas",
  contacts: "Contatos",
  inbox: "Inbox",
  campaigns: "Campanhas",
  catalog: "Catálogo",
  reports: "Relatórios",
  automations: "Automações IA",
  billing: "Billing",
  settings: "Configurações"
};
function UserChip({
  collapsed
}) {
  const {
    Avatar
  } = FluaAS;
  const {
    user
  } = window.FLUA_DATA;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 9,
      padding: collapsed ? 0 : "2px",
      justifyContent: collapsed ? "center" : "flex-start"
    }
  }, /*#__PURE__*/React.createElement(Avatar, {
    name: user.name,
    size: "sm",
    status: "online"
  }), !collapsed && /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-sm)",
      fontWeight: 600,
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, user.name), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)"
    }
  }, user.role)));
}
function AppShell() {
  const {
    SidebarNav,
    CommandBar,
    Icon,
    IconButton,
    Kbd,
    Tooltip
  } = FluaAS;
  const D = window.FLUA_DATA;
  const [view, setView] = React.useState("dashboard");
  const [collapsed, setCollapsed] = React.useState(false);
  const [dark, setDark] = React.useState(false);
  const [cmd, setCmd] = React.useState(false);
  React.useEffect(() => {
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
  }, [dark]);
  React.useEffect(() => {
    const h = e => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setCmd(true);
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, []);
  const go = id => {
    setView(id);
    setCmd(false);
  };
  const cmdGroups = [{
    label: "Ações rápidas",
    items: [{
      label: "Novo lead",
      icon: "plus",
      shortcut: ["⌘", "N"],
      onRun: () => go("contacts")
    }, {
      label: "Novo negócio",
      icon: "git-branch",
      onRun: () => go("pipeline")
    }, {
      label: "Registrar atividade",
      icon: "calendar"
    }, {
      label: "Resumir conversa com IA",
      icon: "sparkles",
      hint: "Beta"
    }]
  }, {
    label: "Navegar",
    items: Object.entries(VIEW_TITLES).map(([id, label]) => ({
      label: "Ir para " + label,
      icon: (D.nav.flatMap(g => g.items).find(i => i.id === id) || {}).icon || "arrow-up-right",
      onRun: () => go(id)
    }))
  }];
  const Views = window;
  let content;
  if (view === "dashboard") content = /*#__PURE__*/React.createElement(Views.DashboardView, null);else if (view === "pipeline") content = /*#__PURE__*/React.createElement(Views.PipelineView, null);else if (view === "contacts") content = /*#__PURE__*/React.createElement(Views.ContactsView, null);else if (view === "inbox") content = /*#__PURE__*/React.createElement(Views.InboxView, null);else if (view === "campaigns") content = /*#__PURE__*/React.createElement(Views.CampaignsView, null);else {
    const icon = (D.nav.flatMap(g => g.items).find(i => i.id === view) || {}).icon || "package";
    content = /*#__PURE__*/React.createElement(Views.EmptyView, {
      title: VIEW_TITLES[view],
      icon: icon
    });
  }
  const noPad = view === "inbox";
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      height: "100vh",
      overflow: "hidden",
      background: "var(--bg-app)"
    }
  }, /*#__PURE__*/React.createElement(SidebarNav, {
    sections: D.nav,
    active: view,
    onSelect: go,
    collapsed: collapsed,
    onToggle: () => setCollapsed(c => !c),
    footer: /*#__PURE__*/React.createElement(UserChip, {
      collapsed: collapsed
    })
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      display: "flex",
      flexDirection: "column",
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("header", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      gap: 12,
      height: "var(--topbar-h)",
      padding: "0 18px",
      flexShrink: 0,
      borderBottom: "1px solid var(--border)",
      background: "var(--surface)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: 600
    }
  }, VIEW_TITLES[view])), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 8
    }
  }, /*#__PURE__*/React.createElement("button", {
    onClick: () => setCmd(true),
    style: {
      display: "flex",
      alignItems: "center",
      gap: 8,
      height: 32,
      padding: "0 10px",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-md)",
      background: "var(--bg-app)",
      cursor: "pointer",
      color: "var(--text-tertiary)",
      fontFamily: "var(--font-sans)",
      fontSize: "var(--text-sm)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: "search",
    size: 15
  }), /*#__PURE__*/React.createElement("span", null, "Buscar\u2026"), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "flex",
      gap: 3,
      marginLeft: 18
    }
  }, /*#__PURE__*/React.createElement(Kbd, null, "\u2318"), /*#__PURE__*/React.createElement(Kbd, null, "K"))), /*#__PURE__*/React.createElement(Tooltip, {
    label: dark ? "Modo claro" : "Modo escuro",
    side: "bottom"
  }, /*#__PURE__*/React.createElement(IconButton, {
    icon: dark ? "sun" : "moon",
    variant: "ghost",
    label: "Tema",
    onClick: () => setDark(d => !d)
  })), /*#__PURE__*/React.createElement(IconButton, {
    icon: "bell",
    variant: "ghost",
    label: "Notifica\xE7\xF5es"
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 2
    }
  }, /*#__PURE__*/React.createElement(FluaAS.Avatar, {
    name: D.user.name,
    size: "sm",
    status: "online"
  })))), /*#__PURE__*/React.createElement("main", {
    style: {
      flex: 1,
      overflowY: noPad ? "hidden" : "auto",
      minHeight: 0
    }
  }, content)), /*#__PURE__*/React.createElement(CommandBar, {
    open: cmd,
    onClose: () => setCmd(false),
    groups: cmdGroups
  }));
}
window.AppShell = AppShell;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/AppShell.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/ContactsView.jsx
try { (() => {
// ContactsView — lead cards grid built on LeadCard. Registers on window.
const FluaCV = window.FluaDesignSystem_2587b4;
function ContactsView() {
  const {
    LeadCard,
    Button,
    Input,
    Select
  } = FluaCV;
  const {
    leads
  } = window.FLUA_DATA;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "20px 24px"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "flex-start",
      justifyContent: "space-between",
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-xl)"
    }
  }, "Contatos"), /*#__PURE__*/React.createElement("p", {
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)",
      marginTop: 3
    }
  }, leads.length, " leads \xB7 atualizados h\xE1 instantes")), /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    size: "md",
    leftIcon: "plus"
  }, "Novo lead")), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 10,
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      maxWidth: 320
    }
  }, /*#__PURE__*/React.createElement(Input, {
    leftIcon: "search",
    placeholder: "Buscar por nome, empresa ou e-mail"
  })), /*#__PURE__*/React.createElement(Select, {
    options: ["Todos os estágios", "Novo", "Qualificado", "Em negociação", "Ganho", "Perdido"]
  }), /*#__PURE__*/React.createElement(Select, {
    options: ["Todos os donos", "Meus leads"]
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "grid",
      gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))",
      gap: 14
    }
  }, leads.map((l, i) => /*#__PURE__*/React.createElement(LeadCard, {
    key: i,
    lead: l,
    onAction: () => {}
  }))));
}
window.ContactsView = ContactsView;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/ContactsView.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/DashboardView.jsx
try { (() => {
// DashboardView — "Visão geral" metrics + pipeline breakdown. Registers on window.
const FluaDV = window.FluaDesignSystem_2587b4;
function MetricCard({
  m
}) {
  const {
    Icon
  } = FluaDV;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      padding: 16,
      boxShadow: "var(--shadow-sm)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between"
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      width: 30,
      height: 30,
      borderRadius: "var(--radius-md)",
      background: "var(--accent-soft)",
      color: "var(--accent)",
      alignItems: "center",
      justifyContent: "center"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: m.icon,
    size: 16
  })), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 3,
      fontSize: "var(--text-xs)",
      fontWeight: 600,
      color: m.up ? "var(--status-won-fg)" : "var(--status-lost-fg)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: m.up ? "trending-up" : "bar-chart",
    size: 13
  }), m.delta)), /*#__PURE__*/React.createElement("div", {
    className: "tnum",
    style: {
      fontSize: "var(--text-2xl)",
      fontWeight: 700,
      letterSpacing: "-0.02em",
      marginTop: 12
    }
  }, m.value), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-xs)",
      color: "var(--text-secondary)",
      marginTop: 2
    }
  }, m.label));
}
function DashboardView() {
  const {
    metrics,
    deals
  } = window.FLUA_DATA;
  const {
    Avatar,
    Badge,
    Icon
  } = FluaDV;
  const stages = [{
    label: "Novo",
    count: 42,
    value: "R$ 318k",
    color: "var(--blue-500)",
    pct: 100
  }, {
    label: "Qualificado",
    count: 28,
    value: "R$ 410k",
    color: "var(--teal-500)",
    pct: 78
  }, {
    label: "Negociação",
    count: 15,
    value: "R$ 392k",
    color: "var(--amber-500)",
    pct: 52
  }, {
    label: "Fechado",
    count: 9,
    value: "R$ 318k",
    color: "var(--green-500)",
    pct: 31
  }];
  return /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "20px 24px"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-xl)"
    }
  }, "Boa tarde, Rafael"), /*#__PURE__*/React.createElement("p", {
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)",
      marginTop: 3
    }
  }, "Aqui est\xE1 o desempenho da sua equipe hoje.")), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "grid",
      gridTemplateColumns: "repeat(4, 1fr)",
      gap: 14,
      marginBottom: 16
    }
  }, metrics.map((m, i) => /*#__PURE__*/React.createElement(MetricCard, {
    key: i,
    m: m
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "grid",
      gridTemplateColumns: "1.4fr 1fr",
      gap: 14
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      padding: 18,
      boxShadow: "var(--shadow-sm)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: 600
    }
  }, "Funil por est\xE1gio"), /*#__PURE__*/React.createElement(Badge, {
    tone: "neutral"
  }, "\xDAltimos 30 dias")), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      flexDirection: "column",
      gap: 14
    }
  }, stages.map(s => /*#__PURE__*/React.createElement("div", {
    key: s.label
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      justifyContent: "space-between",
      fontSize: "var(--text-sm)",
      marginBottom: 5
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      alignItems: "center",
      gap: 7,
      color: "var(--text-primary)",
      fontWeight: 500
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      width: 8,
      height: 8,
      borderRadius: "50%",
      background: s.color
    }
  }), s.label, /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--text-tertiary)",
      fontWeight: 400
    }
  }, "\xB7 ", s.count)), /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      color: "var(--text-secondary)"
    }
  }, s.value)), /*#__PURE__*/React.createElement("div", {
    style: {
      height: 8,
      borderRadius: "var(--radius-full)",
      background: "var(--bg-subtle)",
      overflow: "hidden"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      height: "100%",
      width: s.pct + "%",
      background: s.color,
      borderRadius: "var(--radius-full)"
    }
  })))))), /*#__PURE__*/React.createElement("div", {
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      padding: 18,
      boxShadow: "var(--shadow-sm)"
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: 600,
      marginBottom: 12
    }
  }, "Neg\xF3cios em destaque"), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      flexDirection: "column"
    }
  }, deals.slice(0, 5).map((d, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10,
      padding: "9px 0",
      borderBottom: i < 4 ? "1px solid var(--border-subtle)" : "none"
    }
  }, /*#__PURE__*/React.createElement(Avatar, {
    name: d.owner,
    size: "sm"
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-sm)",
      fontWeight: 500,
      color: "var(--text-primary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, d.name), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)"
    }
  }, d.company)), /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      fontSize: "var(--text-sm)",
      fontWeight: 600
    }
  }, d.value)))))));
}
window.DashboardView = DashboardView;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/DashboardView.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/InboxView.jsx
try { (() => {
// InboxView — two-pane message inbox. Registers on window.
const FluaIV = window.FluaDesignSystem_2587b4;
function InboxView() {
  const {
    Avatar,
    Icon,
    IconButton,
    Button,
    StatusBadge,
    Input
  } = FluaIV;
  const {
    threads,
    messages
  } = window.FLUA_DATA;
  const [active, setActive] = React.useState(0);
  const thread = threads[active];
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      height: "100%",
      minHeight: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      width: 320,
      flexShrink: 0,
      borderRight: "1px solid var(--border)",
      display: "flex",
      flexDirection: "column",
      background: "var(--surface)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "14px 16px 10px",
      borderBottom: "1px solid var(--border-subtle)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginBottom: 10
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-lg)"
    }
  }, "Inbox"), /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-xs)",
      color: "var(--accent)",
      fontWeight: 600
    }
  }, "2 n\xE3o lidas")), /*#__PURE__*/React.createElement(Input, {
    size: "sm",
    leftIcon: "search",
    placeholder: "Buscar conversa"
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      overflowY: "auto"
    }
  }, threads.map((t, i) => /*#__PURE__*/React.createElement("button", {
    key: i,
    onClick: () => setActive(i),
    style: {
      display: "flex",
      gap: 10,
      width: "100%",
      padding: "11px 14px",
      border: "none",
      borderBottom: "1px solid var(--border-subtle)",
      cursor: "pointer",
      textAlign: "left",
      borderLeft: i === active ? "2px solid var(--accent)" : "2px solid transparent",
      background: i === active ? "var(--accent-soft)" : "transparent"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      position: "relative",
      flexShrink: 0
    }
  }, /*#__PURE__*/React.createElement(Avatar, {
    name: t.name,
    size: "md"
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      position: "absolute",
      right: -2,
      bottom: -2,
      width: 15,
      height: 15,
      borderRadius: "50%",
      background: "var(--surface)",
      display: "flex",
      alignItems: "center",
      justifyContent: "center",
      color: "var(--text-tertiary)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: t.channel,
    size: 9
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      justifyContent: "space-between",
      gap: 8
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-sm)",
      fontWeight: t.unread ? 600 : 500,
      color: "var(--text-primary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, t.name), /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)",
      flexShrink: 0
    }
  }, t.time)), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)",
      marginBottom: 2
    }
  }, t.company), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-xs)",
      color: t.unread ? "var(--text-secondary)" : "var(--text-tertiary)",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis"
    }
  }, t.preview)), t.unread && /*#__PURE__*/React.createElement("span", {
    style: {
      width: 7,
      height: 7,
      borderRadius: "50%",
      background: "var(--accent)",
      flexShrink: 0,
      marginTop: 5
    }
  }))))), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      display: "flex",
      flexDirection: "column",
      minWidth: 0,
      background: "var(--bg-app)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      padding: "10px 18px",
      borderBottom: "1px solid var(--border)",
      background: "var(--surface)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10
    }
  }, /*#__PURE__*/React.createElement(Avatar, {
    name: thread.name,
    size: "md",
    status: "online"
  }), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 8
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: 600
    }
  }, thread.name), /*#__PURE__*/React.createElement(StatusBadge, {
    status: "negotiating",
    size: "sm"
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-xs)",
      color: "var(--text-secondary)"
    }
  }, thread.company))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 2
    }
  }, /*#__PURE__*/React.createElement(IconButton, {
    icon: "phone",
    label: "Ligar"
  }), /*#__PURE__*/React.createElement(IconButton, {
    icon: "calendar",
    label: "Agendar"
  }), /*#__PURE__*/React.createElement(IconButton, {
    icon: "sparkles",
    label: "Resumir com IA",
    variant: "outline"
  }), /*#__PURE__*/React.createElement(IconButton, {
    icon: "more-vertical",
    label: "Mais"
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      overflowY: "auto",
      padding: "20px 18px",
      display: "flex",
      flexDirection: "column",
      gap: 10
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      alignSelf: "center",
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)",
      background: "var(--bg-subtle)",
      padding: "3px 10px",
      borderRadius: "999px"
    }
  }, "Hoje"), messages.map((m, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    style: {
      alignSelf: m.from === "me" ? "flex-end" : "flex-start",
      maxWidth: "70%"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "9px 13px",
      borderRadius: 12,
      borderBottomRightRadius: m.from === "me" ? 3 : 12,
      borderBottomLeftRadius: m.from === "me" ? 12 : 3,
      fontSize: "var(--text-base)",
      lineHeight: 1.45,
      background: m.from === "me" ? "var(--accent)" : "var(--surface)",
      color: m.from === "me" ? "#fff" : "var(--text-primary)",
      border: m.from === "me" ? "none" : "1px solid var(--border)"
    }
  }, m.text), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-2xs)",
      color: "var(--text-tertiary)",
      marginTop: 3,
      textAlign: m.from === "me" ? "right" : "left"
    }
  }, m.time)))), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "12px 18px",
      borderTop: "1px solid var(--border)",
      background: "var(--surface)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 8,
      padding: "6px 6px 6px 14px",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      background: "var(--bg-app)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: "sparkles",
    size: 16,
    style: {
      color: "var(--accent)"
    }
  }), /*#__PURE__*/React.createElement("input", {
    placeholder: "Escreva uma resposta ou pe\xE7a \xE0 IA\u2026",
    style: {
      flex: 1,
      border: "none",
      outline: "none",
      background: "transparent",
      fontFamily: "var(--font-sans)",
      fontSize: "var(--text-base)",
      color: "var(--text-primary)"
    }
  }), /*#__PURE__*/React.createElement(Button, {
    size: "sm",
    variant: "primary",
    rightIcon: "arrow-up-right"
  }, "Enviar")))));
}
window.InboxView = InboxView;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/InboxView.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/MiscViews.jsx
try { (() => {
// CampaignsView + generic EmptyView. Registers on window.
const FluaMV = window.FluaDesignSystem_2587b4;
function CampaignsView() {
  const {
    Button,
    StatusBadge,
    IconButton,
    Badge
  } = FluaMV;
  const {
    campaigns
  } = window.FLUA_DATA;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "20px 24px"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "flex-start",
      justifyContent: "space-between",
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-xl)"
    }
  }, "Campanhas"), /*#__PURE__*/React.createElement("p", {
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)",
      marginTop: 3
    }
  }, "3 campanhas ativas")), /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    leftIcon: "plus"
  }, "Nova campanha")), /*#__PURE__*/React.createElement("div", {
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      overflow: "hidden",
      boxShadow: "var(--shadow-sm)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "grid",
      gridTemplateColumns: "2fr 1fr 0.8fr 0.8fr 0.8fr 36px",
      gap: 12,
      padding: "9px 16px",
      borderBottom: "1px solid var(--border)",
      background: "var(--surface-2)"
    }
  }, ["Campanha", "Status", "Enviados", "Abertura", "Resposta", ""].map((h, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    className: "eyebrow"
  }, h))), campaigns.map((c, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    style: {
      display: "grid",
      gridTemplateColumns: "2fr 1fr 0.8fr 0.8fr 0.8fr 36px",
      gap: 12,
      padding: "12px 16px",
      alignItems: "center",
      borderBottom: i < campaigns.length - 1 ? "1px solid var(--border-subtle)" : "none"
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: "var(--text-base)",
      fontWeight: 500
    }
  }, c.name), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(StatusBadge, {
    status: c.status,
    size: "sm"
  })), /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)"
    }
  }, c.sent), /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)"
    }
  }, c.open), /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)"
    }
  }, c.reply), /*#__PURE__*/React.createElement(IconButton, {
    icon: "more-horizontal",
    size: "sm",
    label: "A\xE7\xF5es"
  })))));
}
window.CampaignsView = CampaignsView;
function EmptyView({
  title,
  icon
}) {
  const {
    Icon,
    Button
  } = FluaMV;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "20px 24px"
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-xl)",
      marginBottom: 16
    }
  }, title), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      flexDirection: "column",
      alignItems: "center",
      justifyContent: "center",
      gap: 14,
      height: 360,
      background: "var(--surface)",
      border: "1px dashed var(--border-strong)",
      borderRadius: "var(--radius-lg)"
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      display: "inline-flex",
      width: 52,
      height: 52,
      borderRadius: "var(--radius-lg)",
      background: "var(--accent-soft)",
      color: "var(--accent)",
      alignItems: "center",
      justifyContent: "center"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: icon,
    size: 24
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      textAlign: "center"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-md)",
      fontWeight: 600
    }
  }, title), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)",
      marginTop: 3
    }
  }, "Este m\xF3dulo faz parte do Peitho. Conte\xFAdo de exemplo em breve.")), /*#__PURE__*/React.createElement(Button, {
    variant: "secondary",
    leftIcon: "plus"
  }, "Adicionar")));
}
window.EmptyView = EmptyView;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/MiscViews.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/PipelineView.jsx
try { (() => {
// PipelineView — funnel table built on FunnelRow. Registers on window.
const FluaPV = window.FluaDesignSystem_2587b4;
function PipelineView() {
  const {
    FunnelRow,
    Button,
    IconButton,
    Tabs,
    Badge
  } = FluaPV;
  const {
    deals
  } = window.FLUA_DATA;
  const [tab, setTab] = React.useState("all");
  const total = "R$ 1.241.900";
  return /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "20px 24px"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "flex-start",
      justifyContent: "space-between",
      marginBottom: 16
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h1", {
    style: {
      fontSize: "var(--text-xl)"
    }
  }, "Funil de vendas"), /*#__PURE__*/React.createElement("p", {
    style: {
      fontSize: "var(--text-sm)",
      color: "var(--text-secondary)",
      marginTop: 3
    }
  }, deals.length, " neg\xF3cios abertos \xB7 ", /*#__PURE__*/React.createElement("span", {
    className: "tnum",
    style: {
      color: "var(--text-primary)",
      fontWeight: 600
    }
  }, total), " em pipeline")), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 8
    }
  }, /*#__PURE__*/React.createElement(Button, {
    variant: "secondary",
    size: "md",
    leftIcon: "filter"
  }, "Filtrar"), /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    size: "md",
    leftIcon: "plus"
  }, "Novo neg\xF3cio"))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginBottom: 4
    }
  }, /*#__PURE__*/React.createElement(Tabs, {
    value: tab,
    onChange: setTab,
    items: [{
      value: "all",
      label: "Todos",
      count: deals.length
    }, {
      value: "mine",
      label: "Meus",
      count: 2
    }, {
      value: "stale",
      label: "Parados",
      count: 3
    }]
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 2
    }
  }, /*#__PURE__*/React.createElement(IconButton, {
    icon: "bar-chart",
    label: "Quadro"
  }), /*#__PURE__*/React.createElement(IconButton, {
    icon: "layout-dashboard",
    active: true,
    label: "Lista"
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      background: "var(--surface)",
      border: "1px solid var(--border)",
      borderRadius: "var(--radius-lg)",
      overflow: "hidden",
      boxShadow: "var(--shadow-sm)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "grid",
      gridTemplateColumns: "1.6fr 1.4fr 0.9fr 36px",
      gap: 16,
      padding: "9px 16px",
      borderBottom: "1px solid var(--border)",
      background: "var(--surface-2)"
    }
  }, ["Negócio", "Estágio", "Valor", ""].map((h, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    className: "eyebrow",
    style: {
      textAlign: i === 2 ? "right" : "left"
    }
  }, h))), deals.map((d, i) => /*#__PURE__*/React.createElement(FunnelRow, {
    key: i,
    deal: d,
    onClick: () => {}
  }))));
}
window.PipelineView = PipelineView;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/PipelineView.jsx", error: String((e && e.message) || e) }); }

// ui_kits/crm/data.js
try { (() => {
/* Fake CRM data for the Peitho UI kit. Plain global script. */
window.FLUA_DATA = {
  user: {
    name: "Rafael Mendes",
    role: "Gestor de vendas"
  },
  nav: [{
    label: null,
    items: [{
      id: "dashboard",
      label: "Visão geral",
      icon: "layout-dashboard"
    }, {
      id: "pipeline",
      label: "Funil de vendas",
      icon: "git-branch"
    }, {
      id: "contacts",
      label: "Contatos",
      icon: "users"
    }, {
      id: "inbox",
      label: "Inbox",
      icon: "inbox",
      badge: 4
    }, {
      id: "campaigns",
      label: "Campanhas",
      icon: "megaphone"
    }, {
      id: "catalog",
      label: "Catálogo",
      icon: "package"
    }]
  }, {
    label: "Inteligência",
    items: [{
      id: "reports",
      label: "Relatórios",
      icon: "bar-chart"
    }, {
      id: "automations",
      label: "Automações IA",
      icon: "zap"
    }]
  }, {
    label: "Conta",
    items: [{
      id: "billing",
      label: "Billing",
      icon: "credit-card"
    }, {
      id: "settings",
      label: "Configurações",
      icon: "settings"
    }]
  }],
  metrics: [{
    label: "Pipeline aberto",
    value: "R$ 1,24M",
    delta: "+8,2%",
    up: true,
    icon: "git-branch"
  }, {
    label: "Fechado no mês",
    value: "R$ 318k",
    delta: "+12%",
    up: true,
    icon: "trending-up"
  }, {
    label: "Taxa de conversão",
    value: "24,6%",
    delta: "-1,4%",
    up: false,
    icon: "bar-chart"
  }, {
    label: "Ticket médio",
    value: "R$ 11.900",
    delta: "+3,1%",
    up: true,
    icon: "dollar-sign"
  }],
  deals: [{
    name: "Implantação CRM — 80 assentos",
    company: "Acme Logística",
    value: "R$ 42.000",
    owner: "Bruno Tavares",
    stageIndex: 1
  }, {
    name: "Renovação anual Enterprise",
    company: "Vértice Saúde",
    value: "R$ 120.000",
    owner: "Carla Nunes",
    stageIndex: 2
  }, {
    name: "Upsell módulo de IA",
    company: "Onda Digital",
    value: "R$ 18.900",
    owner: "Diego Alves",
    stageIndex: 3
  }, {
    name: "Migração de planilhas",
    company: "Bloom Cosméticos",
    value: "R$ 9.400",
    owner: "Marina Costa",
    stageIndex: 0
  }, {
    name: "Pacote Suporte Premium",
    company: "Northwind Tecnologia",
    value: "R$ 24.500",
    owner: "Rafael Mendes",
    stageIndex: 2
  }, {
    name: "Expansão multi-equipe",
    company: "Lumina Varejo",
    value: "R$ 88.000",
    owner: "Júlia Souza",
    stageIndex: 1
  }, {
    name: "Integração WhatsApp",
    company: "Praça Delivery",
    value: "R$ 6.200",
    owner: "Diego Alves",
    stageIndex: 0
  }],
  leads: [{
    name: "Marina Costa",
    company: "Bloom Cosméticos",
    status: "new",
    value: "R$ 9.400",
    owner: "Bruno",
    lastActivity: "há 12min",
    tags: ["Inbound", "SP"]
  }, {
    name: "Eduardo Pires",
    company: "Lumina Varejo",
    status: "won",
    value: "R$ 88.000",
    owner: "Júlia",
    lastActivity: "ontem",
    tags: ["Renovação"]
  }, {
    name: "Sofia Almeida",
    company: "Vértice Saúde",
    status: "negotiating",
    value: "R$ 120.000",
    owner: "Carla",
    lastActivity: "há 2h",
    tags: ["Enterprise", "Decisor"]
  }, {
    name: "Tiago Ramos",
    company: "Onda Digital",
    status: "qualified",
    value: "R$ 18.900",
    owner: "Diego",
    lastActivity: "há 1 dia",
    tags: ["Upsell"]
  }, {
    name: "Helena Dias",
    company: "Praça Delivery",
    status: "negotiating",
    value: "R$ 6.200",
    owner: "Rafael",
    lastActivity: "há 3h",
    tags: ["PME"]
  }, {
    name: "Lucas Moreira",
    company: "Acme Logística",
    status: "lost",
    value: "R$ 42.000",
    owner: "Bruno",
    lastActivity: "há 4 dias",
    tags: ["Enterprise"]
  }],
  threads: [{
    name: "Sofia Almeida",
    company: "Vértice Saúde",
    preview: "Perfeito, podemos fechar na condição que conversamos…",
    time: "09:42",
    unread: true,
    channel: "mail"
  }, {
    name: "Helena Dias",
    company: "Praça Delivery",
    preview: "Vocês têm integração com o nosso ERP?",
    time: "09:18",
    unread: true,
    channel: "message-circle"
  }, {
    name: "Tiago Ramos",
    company: "Onda Digital",
    preview: "Recebi a proposta, vou levar pro time amanhã.",
    time: "Ontem",
    unread: false,
    channel: "mail"
  }, {
    name: "Marina Costa",
    company: "Bloom Cosméticos",
    preview: "Obrigada pelo retorno rápido! 🙌",
    time: "Ontem",
    unread: false,
    channel: "message-circle"
  }, {
    name: "Lucas Moreira",
    company: "Acme Logística",
    preview: "Por enquanto vamos seguir com a solução atual.",
    time: "Seg",
    unread: false,
    channel: "phone"
  }],
  messages: [{
    from: "them",
    text: "Oi Rafael! Revisamos a proposta internamente.",
    time: "09:30"
  }, {
    from: "them",
    text: "Perfeito, podemos fechar na condição que conversamos — 12 meses com o módulo de IA incluso?",
    time: "09:42"
  }, {
    from: "me",
    text: "Maravilha, Sofia! Sim, consigo manter o módulo de IA sem custo adicional no primeiro ano.",
    time: "09:45"
  }, {
    from: "me",
    text: "Te envio o contrato ainda hoje para assinatura digital.",
    time: "09:45"
  }],
  campaigns: [{
    name: "Reativação Q2 — Inativos 90d",
    status: "won",
    sent: "4.820",
    open: "38%",
    reply: "6,1%"
  }, {
    name: "Onboarding Enterprise",
    status: "negotiating",
    sent: "312",
    open: "61%",
    reply: "22%"
  }, {
    name: "Black Friday — Lista quente",
    status: "qualified",
    sent: "12.400",
    open: "44%",
    reply: "9,3%"
  }]
};
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/crm/data.js", error: String((e && e.message) || e) }); }

__ds_ns.Button = __ds_scope.Button;

__ds_ns.ICON_PATHS = __ds_scope.ICON_PATHS;

__ds_ns.Icon = __ds_scope.Icon;

__ds_ns.IconButton = __ds_scope.IconButton;

__ds_ns.Kbd = __ds_scope.Kbd;

__ds_ns.CommandBar = __ds_scope.CommandBar;

__ds_ns.PIPELINE_STAGES = __ds_scope.PIPELINE_STAGES;

__ds_ns.FunnelRow = __ds_scope.FunnelRow;

__ds_ns.LeadCard = __ds_scope.LeadCard;

__ds_ns.SidebarNav = __ds_scope.SidebarNav;

__ds_ns.Avatar = __ds_scope.Avatar;

__ds_ns.AvatarGroup = __ds_scope.AvatarGroup;

__ds_ns.Badge = __ds_scope.Badge;

__ds_ns.StatusBadge = __ds_scope.StatusBadge;

__ds_ns.Tag = __ds_scope.Tag;

__ds_ns.Checkbox = __ds_scope.Checkbox;

__ds_ns.Input = __ds_scope.Input;

__ds_ns.Select = __ds_scope.Select;

__ds_ns.Switch = __ds_scope.Switch;

__ds_ns.Card = __ds_scope.Card;

__ds_ns.Tabs = __ds_scope.Tabs;

__ds_ns.Tooltip = __ds_scope.Tooltip;

})();
