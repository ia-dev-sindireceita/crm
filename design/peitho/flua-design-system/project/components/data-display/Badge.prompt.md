A short, copy-pasteable usage note for the data-display components.

```jsx
<Badge tone="accent" icon="zap">IA</Badge>
<StatusBadge status="won" />          {/* Ganho */}
<StatusBadge status="negotiating" />  {/* Em negociação */}
<Tag color="var(--teal-500)" onRemove={() => {}}>Enterprise</Tag>
<Avatar name="Marina Costa" status="online" />
<AvatarGroup people={[{name:"Ana"},{name:"Beto"},{name:"Caio"}]} max={3} />
```

`Badge` tones: neutral · accent · won · lost · nego · info. `StatusBadge` is the canonical deal-stage pill (PT-BR labels). Avatars hash a muted color from the name; pass `src` for a photo.
