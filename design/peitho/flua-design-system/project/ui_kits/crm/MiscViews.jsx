// CampaignsView + generic EmptyView. Registers on window.
const FluaMV = window.FluaDesignSystem_2587b4;

function CampaignsView() {
  const { Button, StatusBadge, IconButton, Badge } = FluaMV;
  const { campaigns } = window.FLUA_DATA;
  return (
    <div style={{ padding: "20px 24px" }}>
      <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", marginBottom: 16 }}>
        <div>
          <h1 style={{ fontSize: "var(--text-xl)" }}>Campanhas</h1>
          <p style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)", marginTop: 3 }}>3 campanhas ativas</p>
        </div>
        <Button variant="primary" leftIcon="plus">Nova campanha</Button>
      </div>
      <div style={{ background: "var(--surface)", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", overflow: "hidden", boxShadow: "var(--shadow-sm)" }}>
        <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr 0.8fr 0.8fr 0.8fr 36px", gap: 12, padding: "9px 16px", borderBottom: "1px solid var(--border)", background: "var(--surface-2)" }}>
          {["Campanha", "Status", "Enviados", "Abertura", "Resposta", ""].map((h, i) => <span key={i} className="eyebrow">{h}</span>)}
        </div>
        {campaigns.map((c, i) => (
          <div key={i} style={{ display: "grid", gridTemplateColumns: "2fr 1fr 0.8fr 0.8fr 0.8fr 36px", gap: 12, padding: "12px 16px", alignItems: "center", borderBottom: i < campaigns.length - 1 ? "1px solid var(--border-subtle)" : "none" }}>
            <span style={{ fontSize: "var(--text-base)", fontWeight: 500 }}>{c.name}</span>
            <span><StatusBadge status={c.status} size="sm" /></span>
            <span className="tnum" style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)" }}>{c.sent}</span>
            <span className="tnum" style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)" }}>{c.open}</span>
            <span className="tnum" style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)" }}>{c.reply}</span>
            <IconButton icon="more-horizontal" size="sm" label="Ações" />
          </div>
        ))}
      </div>
    </div>
  );
}
window.CampaignsView = CampaignsView;

function EmptyView({ title, icon }) {
  const { Icon, Button } = FluaMV;
  return (
    <div style={{ padding: "20px 24px" }}>
      <h1 style={{ fontSize: "var(--text-xl)", marginBottom: 16 }}>{title}</h1>
      <div style={{ display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center", gap: 14,
        height: 360, background: "var(--surface)", border: "1px dashed var(--border-strong)", borderRadius: "var(--radius-lg)" }}>
        <span style={{ display: "inline-flex", width: 52, height: 52, borderRadius: "var(--radius-lg)", background: "var(--accent-soft)", color: "var(--accent)", alignItems: "center", justifyContent: "center" }}>
          <Icon name={icon} size={24} />
        </span>
        <div style={{ textAlign: "center" }}>
          <div style={{ fontSize: "var(--text-md)", fontWeight: 600 }}>{title}</div>
          <div style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)", marginTop: 3 }}>Este módulo faz parte do Peitho. Conteúdo de exemplo em breve.</div>
        </div>
        <Button variant="secondary" leftIcon="plus">Adicionar</Button>
      </div>
    </div>
  );
}
window.EmptyView = EmptyView;
