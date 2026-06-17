// PipelineView — funnel table built on FunnelRow. Registers on window.
const FluaPV = window.FluaDesignSystem_2587b4;

function PipelineView() {
  const { FunnelRow, Button, IconButton, Tabs, Badge } = FluaPV;
  const { deals } = window.FLUA_DATA;
  const [tab, setTab] = React.useState("all");
  const total = "R$ 1.241.900";

  return (
    <div style={{ padding: "20px 24px" }}>
      <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", marginBottom: 16 }}>
        <div>
          <h1 style={{ fontSize: "var(--text-xl)" }}>Funil de vendas</h1>
          <p style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)", marginTop: 3 }}>
            {deals.length} negócios abertos · <span className="tnum" style={{ color: "var(--text-primary)", fontWeight: 600 }}>{total}</span> em pipeline
          </p>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <Button variant="secondary" size="md" leftIcon="filter">Filtrar</Button>
          <Button variant="primary" size="md" leftIcon="plus">Novo negócio</Button>
        </div>
      </div>

      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 4 }}>
        <Tabs value={tab} onChange={setTab} items={[
          { value: "all", label: "Todos", count: deals.length },
          { value: "mine", label: "Meus", count: 2 },
          { value: "stale", label: "Parados", count: 3 },
        ]} />
        <div style={{ display: "flex", gap: 2 }}>
          <IconButton icon="bar-chart" label="Quadro" />
          <IconButton icon="layout-dashboard" active label="Lista" />
        </div>
      </div>

      <div style={{ background: "var(--surface)", border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", overflow: "hidden", boxShadow: "var(--shadow-sm)" }}>
        <div style={{ display: "grid", gridTemplateColumns: "1.6fr 1.4fr 0.9fr 36px", gap: 16, padding: "9px 16px", borderBottom: "1px solid var(--border)", background: "var(--surface-2)" }}>
          {["Negócio", "Estágio", "Valor", ""].map((h, i) => (
            <span key={i} className="eyebrow" style={{ textAlign: i === 2 ? "right" : "left" }}>{h}</span>
          ))}
        </div>
        {deals.map((d, i) => <FunnelRow key={i} deal={d} onClick={() => {}} />)}
      </div>
    </div>
  );
}
window.PipelineView = PipelineView;
