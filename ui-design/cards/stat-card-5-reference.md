# Statistics Card 5 (sean0205) — design reference
Source: https://21st.dev/@sean0205/components/statistics-card-5 (id 4243)
Retrieved 2026-08-11 via 21st MCP (quota 1/2 used).

## Structure (React demo → port target)
```
Card (bg-zinc-900 rounded-2xl, border-0, white text)
├─ CardHeader (border-0, pb-2 pt-6)
│  ├─ CardTitle "Balance"  → text-lg font-semibold text-zinc-400  (label style)
│  └─ CardToolbar          → action Button (bg-zinc-800, icon+text) — optional
└─ CardContent
   ├─ Value row: big value (text-3xl font-bold tracking-tight, white)
   │             + delta (+5.7% text-green-400) inline
   ├─ Divider:   border-b border-zinc-700 mb-6
   └─ Segmented Progress Bar
      └─ segments (width %) each:
         ├─ bar   h-2.5 w-full rounded-sm (color per segment)
         └─ label code (text-xs zinc-400) + percent (text-base font-semibold white)
```

## Key style values (zinc-based, MoonCalendar-compatible)
| Element | Tailwind | Equivalent MoonCalendar token |
|---|---|---|
| Card bg | bg-zinc-900 | --bg-panel #171717 |
| Title | text-zinc-400 | --text-tertiary #9c9c9c |
| Value | text-3xl font-bold tracking-tight | mono 19px (current) |
| Delta + | text-green-400 | --green #4ade80 |
| Divider | border-zinc-700 | --border #2a2a2a |
| Bar h | h-2.5 (10px) rounded-sm | — |
| Segment label | text-xs zinc-400 | --text-tertiary |
| Segment % | font-semibold white | --text-primary |

## FoxRouters mapping ideas
1. **Value + trend inline** — replace .stat-sub row with inline delta (green/red)
2. **Segmented bar** — perfect for quota cards:
   - Freebuff: used 13.0/18 (72%) → green segment + grey remainder
   - Grok: tokens_used / 1M → same pattern
   - Cache Hit % → white/neutral segment
3. **Action button** in card corner (Refresh/sync) — CardToolbar analog

## Files
- preview: cards/5.png
- deps retrieved: button-1.tsx, badge-2.tsx, avatar.tsx, card.tsx (React — reference only)
