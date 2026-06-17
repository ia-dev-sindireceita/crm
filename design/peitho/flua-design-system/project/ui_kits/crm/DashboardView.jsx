// DashboardView — "Visão geral" metrics + pipeline breakdown. Registers on window.
const FluaDV = window.FluaDesignSystem_2587b4;

function MetricCard({ m }) {
  const { Icon } = FluaDV;
  return (
    <div style={{ background: "var(--surface)", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", padding: 16, boxShadow: "var(--shadow-sm)" }}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <span style={{ display: "inline-flex", width: 30, height: 30, borderRadius: "var(--radius-md)", background: "var(--accent-soft)", color: "var(--accent)", alignItems: "center", justifyContent: "center" }}>
          <Icon name={m.icon} size={16} />
        </span>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 3, fontSize: "var(--text-xs)", fontWeight: 600,
          color: m.up ? "var(--status-won-fg)" : "var(--status-lost-fg)" }}>
          <Icon name={m.up ? "trending-up" : "bar-chart"} size={13} />{m.delta}
        </span>
      </div>
      <div className="tnum" style={{ fontSize: "var(--text-2xl)", fontWeight: 700, letterSpacing: "-0.02em", marginTop: 12 }}>{m.value}</div>
      <div style={{ fontSize: "var(--text-xs)", color: "var(--text-secondary)", marginTop: 2 }}>{m.label}</div>
    </div>
  );
}

function DashboardView() {
  const { metrics, deals } = window.FLUA_DATA;
  const { Avatar, Badge, Icon } = FluaDV;
  const stages = [
    { label: "Novo", count: 42, value: "R$ 318k", color: "var(--blue-500)", pct: 100 },
    { label: "Qualificado", count: 28, value: "R$ 410k", color: "var(--teal-500)", pct: 78 },
    { label: "Negociação", count: 15, value: "R$ 392k", color: "var(--amber-500)", pct: 52 },
    { label: "Fechado", count: 9, value: "R$ 318k", color: "var(--green-500)", pct: 31 },
  ];

  return (
    <div style={{ padding: "20px 24px" }}>
      <div style={{ marginBottom: 16 }}>
        <h1 style={{ fontSize: "var(--text-xl)" }}>Boa tarde, Rafael</h1>
        <p style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)", marginTop: 3 }}>Aqui está o desempenho da sua equipe hoje.</p>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 14, marginBottom: 16 }}>
        {metrics.map((m, i) => <MetricCard key={i} m={m} />)}
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "1.4fr 1fr", gap: 14 }}>
        {/* Pipeline funnel */}
        <div style={{ background: "var(--surface)", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", padding: 18, boxShadow: "var(--shadow-sm)" }}>
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 16 }}>
            <h2 style={{ fontSize: "var(--text-md)", fontWeight: 600 }}>Funil por estágio</h2>
            <Badge tone="neutral">Últimos 30 dias</Badge>
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            {stages.map((s) => (
              <div key={s.label}>
                <div style={{ display: "flex", justifyContent: "space-between", fontSize: "var(--text-sm)", marginBottom: 5 }}>
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 7, color: "var(--text-primary)", fontWeight: 500 }}>
                    <span style={{ width: 8, height: 8, borderRadius: "50%", background: s.color }} />{s.label}
                    <span style={{ color: "var(--text-tertiary)", fontWeight: 400 }}>· {s.count}</span>
                  </span>
                  <span className="tnum" style={{ color: "var(--text-secondary)" }}>{s.value}</span>
                </div>
                <div style={{ height: 8, borderRadius: "var(--radius-full)", background: "var(--bg-subtle)", overflow: "hidden" }}>
                  <div style={{ height: "100%", width: s.pct + "%", background: s.color, borderRadius: "var(--radius-full)" }} />
                </div>
              </div>
            ))}
          </div>
        </div>

        {/* Recent / top deals */}
        <div style={{ background: "var(--surface)", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", padding: 18, boxShadow: "var(--shadow-sm)" }}>
          <h2 style={{ fontSize: "var(--text-md)", fontWeight: 600, marginBottom: 12 }}>Negócios em destaque</h2>
          <div style={{ display: "flex", flexDirection: "column" }}>
            {deals.slice(0, 5).map((d, i) => (
              <div key={i} style={{ display: "flex", alignItems: "center", gap: 10, padding: "9px 0", borderBottom: i < 4 ? "1px solid var(--border-subtle)" : "none" }}>
                <Avatar name={d.owner} size="sm" />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ fontSize: "var(--text-sm)", fontWeight: 500, color: "var(--text-primary)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{d.name}</div>
                  <div style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)" }}>{d.company}</div>
                </div>
                <span className="tnum" style={{ fontSize: "var(--text-sm)", fontWeight: 600 }}>{d.value}</span>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}
window.DashboardView = DashboardView;
