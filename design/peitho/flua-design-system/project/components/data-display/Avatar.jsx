import React from "react";

const PALETTE = ["var(--avatar-1)","var(--avatar-2)","var(--avatar-3)","var(--avatar-4)","var(--avatar-5)","var(--avatar-6)"];
const SIZES = { xs: 20, sm: 24, md: 32, lg: 40 };
const FONTS = { xs: 9, sm: 10, md: 12, lg: 15 };

function initials(name = "") {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}
function hashColor(name = "") {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

/** Round avatar — image when `src` given, otherwise hashed-color initials. */
export function Avatar({ name = "", src, size = "md", status, style }) {
  const px = SIZES[size] || SIZES.md;
  const color = hashColor(name);
  return (
    <span style={{ position: "relative", display: "inline-flex", flexShrink: 0, ...style }}>
      {src ? (
        <img
          src={src} alt={name}
          style={{ width: px, height: px, borderRadius: "50%", objectFit: "cover",
            boxShadow: "inset 0 0 0 1px rgba(0,0,0,.06)" }}
        />
      ) : (
        <span
          aria-label={name}
          style={{
            display: "inline-flex", alignItems: "center", justifyContent: "center",
            width: px, height: px, borderRadius: "50%",
            background: color, color: "#fff",
            fontSize: FONTS[size] || 12, fontWeight: "var(--weight-semibold)",
            letterSpacing: "0.01em", userSelect: "none",
          }}
        >
          {initials(name)}
        </span>
      )}
      {status && (
        <span
          style={{
            position: "absolute", right: -1, bottom: -1,
            width: Math.max(8, px * 0.28), height: Math.max(8, px * 0.28),
            borderRadius: "50%", border: "2px solid var(--surface)",
            background: status === "online" ? "var(--green-500)"
              : status === "busy" ? "var(--rose-500)" : "var(--gray-400)",
          }}
        />
      )}
    </span>
  );
}
