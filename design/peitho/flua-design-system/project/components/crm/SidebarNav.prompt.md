The five Peitho CRM signature components.

```jsx
<SidebarNav
  sections={[{ items: [
    { id:"pipeline", label:"Funil",    icon:"git-branch" },
    { id:"contacts", label:"Contatos", icon:"users" },
    { id:"inbox",    label:"Inbox",    icon:"inbox", badge:3 },
  ]}]}
  active="pipeline" collapsed={false} onToggle={()=>{}} onSelect={()=>{}}
/>

<LeadCard lead={{ name:"Marina Costa", company:"Northwind SA", status:"negotiating",
  value:"R$ 24.500", owner:"Rafael", lastActivity:"há 2h", tags:["Enterprise","Inbound"] }} />

<FunnelRow deal={{ name:"Renovação anual", company:"Acme", value:"R$ 88.000",
  owner:"Júlia", stageIndex:2 }} />

<CommandBar open={open} onClose={()=>setOpen(false)} groups={[
  { label:"Ações", items:[{ label:"Novo lead", icon:"plus", shortcut:["⌘","N"], onRun(){} }] },
]} />
```

`SidebarNav` collapses to icon-only (tooltips appear). `StatusBadge` is the canonical Ganho/Perdido/Em negociação pill. `CommandBar` filters on type, arrow-keys + Enter to run.
