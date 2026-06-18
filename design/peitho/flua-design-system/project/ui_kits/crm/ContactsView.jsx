// ContactsView — lead cards grid built on LeadCard. Registers on window.
const FluaCV = window.FluaDesignSystem_2587b4;

function ContactsView() {
  const { LeadCard, Button, Input, Select } = FluaCV;
  const { leads } = window.FLUA_DATA;

  return (
    <div style={{ padding: "20px 24px" }}>
      <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", marginBottom: 16 }}>
        <div>
          <h1 style={{ fontSize: "var(--text-xl)" }}>Contatos</h1>
          <p style={{ fontSize: "var(--text-sm)", color: "var(--text-secondary)", marginTop: 3 }}>
            {leads.length} leads · atualizados há instantes
          </p>
        </div>
        <Button variant="primary" size="md" leftIcon="plus">Novo lead</Button>
      </div>

      <div style={{ display: "flex", gap: 10, marginBottom: 16 }}>
        <div style={{ flex: 1, maxWidth: 320 }}>
          <Input leftIcon="search" placeholder="Buscar por nome, empresa ou e-mail" />
        </div>
        <Select options={["Todos os estágios", "Novo", "Qualificado", "Em negociação", "Ganho", "Perdido"]} />
        <Select options={["Todos os donos", "Meus leads"]} />
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))", gap: 14 }}>
        {leads.map((l, i) => <LeadCard key={i} lead={l} onAction={() => {}} />)}
      </div>
    </div>
  );
}
window.ContactsView = ContactsView;
