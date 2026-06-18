import React from "react";
import { Avatar } from "../data-display/Avatar.jsx";
import { StatusBadge } from "../data-display/StatusBadge.jsx";
import { IconButton } from "../core/IconButton.jsx";
import { Icon } from "../core/Icon.jsx";

/** Contact / lead card: avatar, name, company, status, value, quick actions. */
export function LeadCard({ lead, onAction, style }) {
  const { name, company, status, value, owner, email, lastActivity, tags } = lead || {};
  const [hover, setHover] = React.useState(false);
  return (
    <div
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        background: "var(--surface)", border: "1px solid var(--border)",
        borderColor: hover ? "var(--border-strong)" : "var(--border)",
        borderRadius: "var(--radius-lg)", padding: 14,
        boxShadow: hover ? "var(--shadow-md)" : "var(--shadow-sm)",
        transition: "box-shadow .16s ease, border-color .16s ease",
        ...style,
      }}
    >
      <div style={{ display: "flex", alignItems: "flex-start", gap: 11 }}>
        <Avatar name={name} size="md" />
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <span style={{ fontSize: "var(--text-md)", fontWeight: "var(--weight-semibold)",
              color: "var(--text-primary)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{name}</span>
            {status && <StatusBadge status={status} size="sm" />}
          </div>
          <div style={{ display: "flex", alignItems: "center", gap: 5, marginTop: 2,
            fontSize: "var(--text-sm)", color: "var(--text-secondary)" }}>
            <Icon name="building" size={13} style={{ color: "var(--text-tertiary)" }} />
            <span style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{company}</span>
          </div>
        </div>
        {value != null && (
          <div style={{ textAlign: "right", flexShrink: 0 }}>
            <div className="tnum" style={{ fontSize: "var(--text-md)", fontWeight: "var(--weight-semibold)",
              color: "var(--text-primary)" }}>{value}</div>
            <div style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)" }}>valor</div>
          </div>
        )}
      </div>

      {tags && tags.length > 0 && (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 5, marginTop: 11 }}>
          {tags.map((t) => (
            <span key={t} style={{ fontSize: "var(--text-2xs)", color: "var(--text-secondary)",
              background: "var(--bg-subtle)", padding: "2px 7px", borderRadius: "var(--radius-sm)" }}>{t}</span>
          ))}
        </div>
      )}

      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between",
        marginTop: 12, paddingTop: 11, borderTop: "1px solid var(--border-subtle)" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 6, minWidth: 0 }}>
          {owner && <Avatar name={owner} size="xs" />}
          {lastActivity && (
            <span style={{ display: "inline-flex", alignItems: "center", gap: 4,
              fontSize: "var(--text-2xs)", color: "var(--text-tertiary)" }}>
              <Icon name="clock" size={11} />{lastActivity}
            </span>
          )}
        </div>
        <div style={{ display: "flex", gap: 2, opacity: hover ? 1 : 0.55, transition: "opacity .14s ease" }}>
          <IconButton icon="phone" size="sm" label="Ligar" onClick={() => onAction && onAction("call", lead)} />
          <IconButton icon="mail" size="sm" label="E-mail" onClick={() => onAction && onAction("mail", lead)} />
          <IconButton icon="message-circle" size="sm" label="Mensagem" onClick={() => onAction && onAction("message", lead)} />
        </div>
      </div>
    </div>
  );
}
