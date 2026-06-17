import React from "react";
import { Avatar } from "./Avatar.jsx";

const SIZES = { xs: 20, sm: 24, md: 32, lg: 40 };

/** Overlapping stack of avatars with a +N overflow chip. */
export function AvatarGroup({ people = [], size = "sm", max = 4, style }) {
  const px = SIZES[size] || SIZES.sm;
  const shown = people.slice(0, max);
  const extra = people.length - shown.length;
  const overlap = Math.round(px * 0.32);
  return (
    <div style={{ display: "inline-flex", alignItems: "center", ...style }}>
      {shown.map((p, i) => (
        <span key={i} style={{ marginLeft: i === 0 ? 0 : -overlap, borderRadius: "50%",
          boxShadow: "0 0 0 2px var(--surface)", position: "relative", zIndex: i }}>
          <Avatar name={p.name} src={p.src} size={size} />
        </span>
      ))}
      {extra > 0 && (
        <span
          style={{
            marginLeft: -overlap, display: "inline-flex", alignItems: "center", justifyContent: "center",
            width: px, height: px, borderRadius: "50%", background: "var(--bg-subtle)",
            color: "var(--text-secondary)", fontSize: size === "lg" ? 13 : 10,
            fontWeight: "var(--weight-semibold)", boxShadow: "0 0 0 2px var(--surface)",
            position: "relative", zIndex: shown.length,
          }}
        >
          +{extra}
        </span>
      )}
    </div>
  );
}
