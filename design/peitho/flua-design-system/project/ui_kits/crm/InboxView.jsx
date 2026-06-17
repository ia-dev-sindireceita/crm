// InboxView — two-pane message inbox. Registers on window.
const FluaIV = window.FluaDesignSystem_2587b4;

function InboxView() {
  const { Avatar, Icon, IconButton, Button, StatusBadge, Input } = FluaIV;
  const { threads, messages } = window.FLUA_DATA;
  const [active, setActive] = React.useState(0);
  const thread = threads[active];

  return (
    <div style={{ display: "flex", height: "100%", minHeight: 0 }}>
      {/* Thread list */}
      <div style={{ width: 320, flexShrink: 0, borderRight: "1px solid var(--border)", display: "flex", flexDirection: "column", background: "var(--surface)" }}>
        <div style={{ padding: "14px 16px 10px", borderBottom: "1px solid var(--border-subtle)" }}>
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 10 }}>
            <h1 style={{ fontSize: "var(--text-lg)" }}>Inbox</h1>
            <span style={{ fontSize: "var(--text-xs)", color: "var(--accent)", fontWeight: 600 }}>2 não lidas</span>
          </div>
          <Input size="sm" leftIcon="search" placeholder="Buscar conversa" />
        </div>
        <div style={{ flex: 1, overflowY: "auto" }}>
          {threads.map((t, i) => (
            <button key={i} onClick={() => setActive(i)}
              style={{ display: "flex", gap: 10, width: "100%", padding: "11px 14px", border: "none",
                borderBottom: "1px solid var(--border-subtle)", cursor: "pointer", textAlign: "left",
                borderLeft: i === active ? "2px solid var(--accent)" : "2px solid transparent",
                background: i === active ? "var(--accent-soft)" : "transparent" }}>
              <div style={{ position: "relative", flexShrink: 0 }}>
                <Avatar name={t.name} size="md" />
                <span style={{ position: "absolute", right: -2, bottom: -2, width: 15, height: 15, borderRadius: "50%",
                  background: "var(--surface)", display: "flex", alignItems: "center", justifyContent: "center", color: "var(--text-tertiary)" }}>
                  <Icon name={t.channel} size={9} />
                </span>
              </div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
                  <span style={{ fontSize: "var(--text-sm)", fontWeight: t.unread ? 600 : 500, color: "var(--text-primary)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{t.name}</span>
                  <span style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)", flexShrink: 0 }}>{t.time}</span>
                </div>
                <div style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)", marginBottom: 2 }}>{t.company}</div>
                <div style={{ fontSize: "var(--text-xs)", color: t.unread ? "var(--text-secondary)" : "var(--text-tertiary)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{t.preview}</div>
              </div>
              {t.unread && <span style={{ width: 7, height: 7, borderRadius: "50%", background: "var(--accent)", flexShrink: 0, marginTop: 5 }} />}
            </button>
          ))}
        </div>
      </div>

      {/* Conversation */}
      <div style={{ flex: 1, display: "flex", flexDirection: "column", minWidth: 0, background: "var(--bg-app)" }}>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "10px 18px", borderBottom: "1px solid var(--border)", background: "var(--surface)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            <Avatar name={thread.name} size="md" status="online" />
            <div>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <span style={{ fontSize: "var(--text-md)", fontWeight: 600 }}>{thread.name}</span>
                <StatusBadge status="negotiating" size="sm" />
              </div>
              <div style={{ fontSize: "var(--text-xs)", color: "var(--text-secondary)" }}>{thread.company}</div>
            </div>
          </div>
          <div style={{ display: "flex", gap: 2 }}>
            <IconButton icon="phone" label="Ligar" />
            <IconButton icon="calendar" label="Agendar" />
            <IconButton icon="sparkles" label="Resumir com IA" variant="outline" />
            <IconButton icon="more-vertical" label="Mais" />
          </div>
        </div>

        <div style={{ flex: 1, overflowY: "auto", padding: "20px 18px", display: "flex", flexDirection: "column", gap: 10 }}>
          <div style={{ alignSelf: "center", fontSize: "var(--text-2xs)", color: "var(--text-tertiary)", background: "var(--bg-subtle)", padding: "3px 10px", borderRadius: "999px" }}>Hoje</div>
          {messages.map((m, i) => (
            <div key={i} style={{ alignSelf: m.from === "me" ? "flex-end" : "flex-start", maxWidth: "70%" }}>
              <div style={{ padding: "9px 13px", borderRadius: 12,
                borderBottomRightRadius: m.from === "me" ? 3 : 12, borderBottomLeftRadius: m.from === "me" ? 12 : 3,
                fontSize: "var(--text-base)", lineHeight: 1.45,
                background: m.from === "me" ? "var(--accent)" : "var(--surface)",
                color: m.from === "me" ? "#fff" : "var(--text-primary)",
                border: m.from === "me" ? "none" : "1px solid var(--border)" }}>{m.text}</div>
              <div style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)", marginTop: 3, textAlign: m.from === "me" ? "right" : "left" }}>{m.time}</div>
            </div>
          ))}
        </div>

        <div style={{ padding: "12px 18px", borderTop: "1px solid var(--border)", background: "var(--surface)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "6px 6px 6px 14px", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", background: "var(--bg-app)" }}>
            <Icon name="sparkles" size={16} style={{ color: "var(--accent)" }} />
            <input placeholder="Escreva uma resposta ou peça à IA…" style={{ flex: 1, border: "none", outline: "none", background: "transparent", fontFamily: "var(--font-sans)", fontSize: "var(--text-base)", color: "var(--text-primary)" }} />
            <Button size="sm" variant="primary" rightIcon="arrow-up-right">Enviar</Button>
          </div>
        </div>
      </div>
    </div>
  );
}
window.InboxView = InboxView;
