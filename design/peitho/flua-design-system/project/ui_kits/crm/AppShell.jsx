// AppShell — sidebar + topbar + content + ⌘K. Composes the views. Registers on window.
const FluaAS = window.FluaDesignSystem_2587b4;

const VIEW_TITLES = {
  dashboard: "Visão geral", pipeline: "Funil de vendas", contacts: "Contatos",
  inbox: "Inbox", campaigns: "Campanhas", catalog: "Catálogo",
  reports: "Relatórios", automations: "Automações IA", billing: "Billing", settings: "Configurações",
};

function UserChip({ collapsed }) {
  const { Avatar } = FluaAS;
  const { user } = window.FLUA_DATA;
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 9, padding: collapsed ? 0 : "2px", justifyContent: collapsed ? "center" : "flex-start" }}>
      <Avatar name={user.name} size="sm" status="online" />
      {!collapsed && (
        <div style={{ minWidth: 0 }}>
          <div style={{ fontSize: "var(--text-sm)", fontWeight: 600, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{user.name}</div>
          <div style={{ fontSize: "var(--text-2xs)", color: "var(--text-tertiary)" }}>{user.role}</div>
        </div>
      )}
    </div>
  );
}

function AppShell() {
  const { SidebarNav, CommandBar, Icon, IconButton, Kbd, Tooltip } = FluaAS;
  const D = window.FLUA_DATA;
  const [view, setView] = React.useState("dashboard");
  const [collapsed, setCollapsed] = React.useState(false);
  const [dark, setDark] = React.useState(false);
  const [cmd, setCmd] = React.useState(false);

  React.useEffect(() => {
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
  }, [dark]);

  React.useEffect(() => {
    const h = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); setCmd(true); }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, []);

  const go = (id) => { setView(id); setCmd(false); };
  const cmdGroups = [
    { label: "Ações rápidas", items: [
      { label: "Novo lead", icon: "plus", shortcut: ["⌘", "N"], onRun: () => go("contacts") },
      { label: "Novo negócio", icon: "git-branch", onRun: () => go("pipeline") },
      { label: "Registrar atividade", icon: "calendar" },
      { label: "Resumir conversa com IA", icon: "sparkles", hint: "Beta" },
    ]},
    { label: "Navegar", items: Object.entries(VIEW_TITLES).map(([id, label]) => ({
      label: "Ir para " + label, icon: (D.nav.flatMap(g => g.items).find(i => i.id === id) || {}).icon || "arrow-up-right", onRun: () => go(id),
    })) },
  ];

  const Views = window;
  let content;
  if (view === "dashboard") content = <Views.DashboardView />;
  else if (view === "pipeline") content = <Views.PipelineView />;
  else if (view === "contacts") content = <Views.ContactsView />;
  else if (view === "inbox") content = <Views.InboxView />;
  else if (view === "campaigns") content = <Views.CampaignsView />;
  else {
    const icon = (D.nav.flatMap(g => g.items).find(i => i.id === view) || {}).icon || "package";
    content = <Views.EmptyView title={VIEW_TITLES[view]} icon={icon} />;
  }

  const noPad = view === "inbox";

  return (
    <div style={{ display: "flex", height: "100vh", overflow: "hidden", background: "var(--bg-app)" }}>
      <SidebarNav
        sections={D.nav} active={view} onSelect={go}
        collapsed={collapsed} onToggle={() => setCollapsed(c => !c)}
        footer={<UserChip collapsed={collapsed} />}
      />

      <div style={{ flex: 1, display: "flex", flexDirection: "column", minWidth: 0 }}>
        {/* Topbar */}
        <header style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12,
          height: "var(--topbar-h)", padding: "0 18px", flexShrink: 0,
          borderBottom: "1px solid var(--border)", background: "var(--surface)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            <h2 style={{ fontSize: "var(--text-md)", fontWeight: 600 }}>{VIEW_TITLES[view]}</h2>
          </div>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <button onClick={() => setCmd(true)} style={{ display: "flex", alignItems: "center", gap: 8, height: 32, padding: "0 10px",
              border: "1px solid var(--border)", borderRadius: "var(--radius-md)", background: "var(--bg-app)", cursor: "pointer",
              color: "var(--text-tertiary)", fontFamily: "var(--font-sans)", fontSize: "var(--text-sm)" }}>
              <Icon name="search" size={15} />
              <span>Buscar…</span>
              <span style={{ display: "flex", gap: 3, marginLeft: 18 }}><Kbd>⌘</Kbd><Kbd>K</Kbd></span>
            </button>
            <Tooltip label={dark ? "Modo claro" : "Modo escuro"} side="bottom">
              <IconButton icon={dark ? "sun" : "moon"} variant="ghost" label="Tema" onClick={() => setDark(d => !d)} />
            </Tooltip>
            <IconButton icon="bell" variant="ghost" label="Notificações" />
            <span style={{ marginLeft: 2 }}><FluaAS.Avatar name={D.user.name} size="sm" status="online" /></span>
          </div>
        </header>

        <main style={{ flex: 1, overflowY: noPad ? "hidden" : "auto", minHeight: 0 }}>
          {content}
        </main>
      </div>

      <CommandBar open={cmd} onClose={() => setCmd(false)} groups={cmdGroups} />
    </div>
  );
}
window.AppShell = AppShell;
