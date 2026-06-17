/* Fake CRM data for the Peitho UI kit. Plain global script. */
window.FLUA_DATA = {
  user: { name: "Rafael Mendes", role: "Gestor de vendas" },

  nav: [
    { label: null, items: [
      { id: "dashboard", label: "Visão geral", icon: "layout-dashboard" },
      { id: "pipeline",  label: "Funil de vendas", icon: "git-branch" },
      { id: "contacts",  label: "Contatos", icon: "users" },
      { id: "inbox",     label: "Inbox", icon: "inbox", badge: 4 },
      { id: "campaigns", label: "Campanhas", icon: "megaphone" },
      { id: "catalog",   label: "Catálogo", icon: "package" },
    ]},
    { label: "Inteligência", items: [
      { id: "reports",   label: "Relatórios", icon: "bar-chart" },
      { id: "automations", label: "Automações IA", icon: "zap" },
    ]},
    { label: "Conta", items: [
      { id: "billing",   label: "Billing", icon: "credit-card" },
      { id: "settings",  label: "Configurações", icon: "settings" },
    ]},
  ],

  metrics: [
    { label: "Pipeline aberto", value: "R$ 1,24M", delta: "+8,2%", up: true, icon: "git-branch" },
    { label: "Fechado no mês",  value: "R$ 318k",  delta: "+12%",  up: true, icon: "trending-up" },
    { label: "Taxa de conversão", value: "24,6%",  delta: "-1,4%", up: false, icon: "bar-chart" },
    { label: "Ticket médio",    value: "R$ 11.900", delta: "+3,1%", up: true, icon: "dollar-sign" },
  ],

  deals: [
    { name: "Implantação CRM — 80 assentos", company: "Acme Logística", value: "R$ 42.000", owner: "Bruno Tavares", stageIndex: 1 },
    { name: "Renovação anual Enterprise", company: "Vértice Saúde", value: "R$ 120.000", owner: "Carla Nunes", stageIndex: 2 },
    { name: "Upsell módulo de IA", company: "Onda Digital", value: "R$ 18.900", owner: "Diego Alves", stageIndex: 3 },
    { name: "Migração de planilhas", company: "Bloom Cosméticos", value: "R$ 9.400", owner: "Marina Costa", stageIndex: 0 },
    { name: "Pacote Suporte Premium", company: "Northwind Tecnologia", value: "R$ 24.500", owner: "Rafael Mendes", stageIndex: 2 },
    { name: "Expansão multi-equipe", company: "Lumina Varejo", value: "R$ 88.000", owner: "Júlia Souza", stageIndex: 1 },
    { name: "Integração WhatsApp", company: "Praça Delivery", value: "R$ 6.200", owner: "Diego Alves", stageIndex: 0 },
  ],

  leads: [
    { name: "Marina Costa", company: "Bloom Cosméticos", status: "new", value: "R$ 9.400", owner: "Bruno", lastActivity: "há 12min", tags: ["Inbound", "SP"] },
    { name: "Eduardo Pires", company: "Lumina Varejo", status: "won", value: "R$ 88.000", owner: "Júlia", lastActivity: "ontem", tags: ["Renovação"] },
    { name: "Sofia Almeida", company: "Vértice Saúde", status: "negotiating", value: "R$ 120.000", owner: "Carla", lastActivity: "há 2h", tags: ["Enterprise", "Decisor"] },
    { name: "Tiago Ramos", company: "Onda Digital", status: "qualified", value: "R$ 18.900", owner: "Diego", lastActivity: "há 1 dia", tags: ["Upsell"] },
    { name: "Helena Dias", company: "Praça Delivery", status: "negotiating", value: "R$ 6.200", owner: "Rafael", lastActivity: "há 3h", tags: ["PME"] },
    { name: "Lucas Moreira", company: "Acme Logística", status: "lost", value: "R$ 42.000", owner: "Bruno", lastActivity: "há 4 dias", tags: ["Enterprise"] },
  ],

  threads: [
    { name: "Sofia Almeida", company: "Vértice Saúde", preview: "Perfeito, podemos fechar na condição que conversamos…", time: "09:42", unread: true, channel: "mail" },
    { name: "Helena Dias", company: "Praça Delivery", preview: "Vocês têm integração com o nosso ERP?", time: "09:18", unread: true, channel: "message-circle" },
    { name: "Tiago Ramos", company: "Onda Digital", preview: "Recebi a proposta, vou levar pro time amanhã.", time: "Ontem", unread: false, channel: "mail" },
    { name: "Marina Costa", company: "Bloom Cosméticos", preview: "Obrigada pelo retorno rápido! 🙌", time: "Ontem", unread: false, channel: "message-circle" },
    { name: "Lucas Moreira", company: "Acme Logística", preview: "Por enquanto vamos seguir com a solução atual.", time: "Seg", unread: false, channel: "phone" },
  ],

  messages: [
    { from: "them", text: "Oi Rafael! Revisamos a proposta internamente.", time: "09:30" },
    { from: "them", text: "Perfeito, podemos fechar na condição que conversamos — 12 meses com o módulo de IA incluso?", time: "09:42" },
    { from: "me", text: "Maravilha, Sofia! Sim, consigo manter o módulo de IA sem custo adicional no primeiro ano.", time: "09:45" },
    { from: "me", text: "Te envio o contrato ainda hoje para assinatura digital.", time: "09:45" },
  ],

  campaigns: [
    { name: "Reativação Q2 — Inativos 90d", status: "won", sent: "4.820", open: "38%", reply: "6,1%" },
    { name: "Onboarding Enterprise", status: "negotiating", sent: "312", open: "61%", reply: "22%" },
    { name: "Black Friday — Lista quente", status: "qualified", sent: "12.400", open: "44%", reply: "9,3%" },
  ],
};
