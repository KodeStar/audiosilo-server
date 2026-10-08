# Shelf: AudioSilo admin console design system (Direction B)

> The admin console as a well-kept private library. The collection is the hero;
> administration happens in the context of the books and the people who listen to them.

Target stack: **shadcn/ui on Base UI + Tailwind v4 + React 19 + Vite + TypeScript**, built to static files,
embedded in the Go binary and served under `script-src 'self'; style-src 'self'`. Companions: TanStack
Table + Virtual, TanStack Query, cmdk, Base UI Toast (not sonner), Base UI Drawer, react-hook-form + zod,
Recharts via shadcn `chart` (without its inline `<style>`), @headless-tree/react, dnd-kit, lucide-react,
fontsource for fonts.

This copy (in `audiosilo-server/admin-ui/`) is authoritative for the built console; the original
lives with the prototype in the workspace's `design/admin-redesign/shelf/`. The prototype
(`index.html`, route `#styleguide`) shows the intended look; this file wins where they differ, and
section 13 lists the CSP rules every component must follow.

---

## 1. Principles

1. **Covers lead, chrome recedes.** Every surface that lists books shows covers first. Chrome is flat
   with hairline borders so the art carries the colour. Covers (and floating layers) are the only things
   with shadows.
2. **Edit in place, in context.** Metadata is fixed on the book page itself: click a value, type, done.
   Every value shows where it came from (file tag / path / edited / community) and can be reverted.
3. **Safe by default, and honest about it.** The server is read-only toward files. Anything that touches
   disk lives in a fenced "Files on disk" area, is gated by a Settings switch, previews before/after,
   and says that progress follows via move-tracking. Safety stops (an unmounted NAS) are celebrated
   ("Nothing was deleted"), not shown as failures.
4. **People, not accounts.** Users have faces (gradient monogram avatars), progress rings and "listening
   now". Access is phrased as "What can Sam listen to?", previewed as covers.
5. **One pink thing per view.** Brand pink (`--brand`, the player's `#db2777`) marks selection, progress
   and the single most important highlight. Primary buttons are deep ink. If two things on screen are
   pink and neither is progress/selection, one of them is wrong.

---

## 2. Information architecture

```
Top bar (64px, sticky, translucent):  [mark + server name + health line]  Library  People  Activity  Health  Server   [ Omnisearch ⌘K ]   bell  theme  avatar
Sub bar (48px, per destination):      Title  [segmented control of sections]                                                    [contextual action]
```

| Destination     | Sections (segmented control)                                                                                      | Routes                      |
| --------------- | ----------------------------------------------------------------------------------------------------------------- | --------------------------- |
| Home (the mark) | none: greeting, safety notices, live now, stat tiles, recently added, what happened, needs attention, server card | `/admin/`                   |
| Library         | Books · Authors · Series · Narrators · Folders · Libraries                                                        | `/admin/library/{section}`  |
| People          | People · Invites · Shares · Devices                                                                               | `/admin/people/{section}`   |
| Activity        | Overview · Live now · Sessions · Year in listening                                                                | `/admin/activity/{section}` |
| Health          | Issues · Jobs · System                                                                                            | `/admin/health/{section}`   |
| Server          | Settings · Logs · Audit log · About                                                                               | `/admin/server/{section}`   |

- Settings uses an in-page topic list (`?topic=`; a column of links beside the topic, a scrolling row
  on phones) for its topics: General, Network & HTTPS, Players & app links, Community metadata,
  Transcoding, Demo mode, Backups, Notifications. Logs, Audit log and About are
  records, not settings, so they live only in the sub bar. **Each setting lives in exactly one
  place.** A setting the environment or the desktop app sets is shown locked with what sets it; one
  read only at start says "Restart to apply"; each card saves only its changed fields.
- Mobile (<720px): the five destinations move to a bottom tab bar; the sub bar becomes a horizontally
  scrolling segmented control; the omnisearch stays in the top bar.
- Detail pages (`#book`, `#user`) replace the segmented control with a breadcrumb + back button.
- Real paths under `/admin` (TanStack Router, `basepath: '/admin'`); the first section of a
  destination has no segment (`/admin/library` is Books). Sub-state (selected book, tab, filters)
  lives in search params so every view deep-links. The server serves `index.html` for any
  `/admin/...` path that isn't a file.

---

## 3. Tokens (`globals.css`)

```css
@import 'tailwindcss';
@import '@fontsource-variable/bricolage-grotesque';
@import '@fontsource-variable/figtree';
@import '@fontsource/jetbrains-mono/400.css';
@import '@fontsource/jetbrains-mono/600.css';

@custom-variant dark (&:where([data-theme=dark], [data-theme=dark] *));

:root {
  --radius: 14px;

  --background: #f5f7fa; /* cool porcelain */
  --foreground: #121c36; /* ink-navy */
  --card: #ffffff;
  --card-foreground: #121c36;
  --popover: #ffffff;
  --popover-foreground: #121c36;
  --primary: #15203d; /* deep ink: primary buttons, toasts, bulk bar */
  --primary-foreground: #f5f7fa;
  --secondary: #eaedf3;
  --secondary-foreground: #18244a;
  --muted: #eef1f5;
  --muted-foreground: #5b6680;
  --subtle-foreground: #8590a6;
  --accent: #e8ecf3; /* hover on ghost items */
  --accent-foreground: #121c36;
  --destructive: #c42b3c;
  --destructive-soft: #fde8ea;
  --border: #e0e5ed;
  --border-strong: #cdd4df;
  --input: #d4dae4;
  --ring: #db2777;

  --brand: #db2777; /* the player pink */
  --brand-foreground: #ffffff;
  --brand-soft: #fce7f1;
  --brand-ink: #a3195a; /* pink text on light surfaces (AA) */

  --success: #0d7f5a;
  --success-soft: #e1f5ec;
  --warning: #a86206;
  --warning-soft: #fdf1dc;
  --info: #2c56c9;
  --info-soft: #e6edfd;

  /* field provenance */
  --prov-tag: #4b5e86;
  --prov-tag-soft: #e9eef8;
  --prov-path: #0f7b74;
  --prov-path-soft: #e0f3f1;
  --prov-edit: #b81c63;
  --prov-edit-soft: #fce7f1;
  --prov-community: #6a3fd4;
  --prov-community-soft: #efe9fd;

  /* charts: categorical, fixed order, CVD-validated (light surface) */
  --chart-1: #db2777;
  --chart-2: #3b5bdb;
  --chart-3: #0d9488;
  --chart-4: #d97706;
  --chart-5: #7c3aed;
  /* sequential (heatmaps): one hue, light -> dark */
  --seq-0: #f3f4f8;
  --seq-1: #fbd5e6;
  --seq-2: #f5a3c7;
  --seq-3: #e8649f;
  --seq-4: #cc2b78;
  --seq-5: #8f1550;

  --sidebar: #ffffff;
  --sidebar-foreground: #121c36;
  --sidebar-primary: #15203d;
  --sidebar-primary-foreground: #f5f7fa;
  --sidebar-accent: #eef1f5;
  --sidebar-accent-foreground: #121c36;
  --sidebar-border: #e0e5ed;
  --sidebar-ring: #db2777;

  --topbar: rgb(245 247 250 / 0.82);
  --overlay: rgb(14 20 38 / 0.38);
  --shadow-cover:
    0 1px 1px rgb(18 28 54 / 0.1), 0 4px 8px -2px rgb(18 28 54 / 0.14),
    0 16px 28px -10px rgb(18 28 54 / 0.3);
  --shadow-cover-hover:
    0 2px 2px rgb(18 28 54 / 0.1), 0 10px 18px -6px rgb(18 28 54 / 0.2),
    0 28px 44px -14px rgb(18 28 54 / 0.38);
  --shadow-overlay: 0 24px 64px -16px rgb(14 22 48 / 0.3), 0 2px 6px rgb(14 22 48 / 0.08);

  --font-display:
    'Bricolage Grotesque Variable', 'Figtree Variable', ui-sans-serif, system-ui, sans-serif; /* fontsource-variable family names */
  --font-sans: 'Figtree Variable', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif;
  --font-mono: 'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, monospace;

  --ease-out: cubic-bezier(0.2, 0.8, 0.2, 1);
  --ease-spring: cubic-bezier(0.34, 1.36, 0.64, 1);
  --dur-1: 120ms;
  --dur-2: 200ms;
  --dur-3: 320ms;
}

/* Dark: deep ink where covers glow. Apply for data-theme=dark, and for system dark unless data-theme=light. */
[data-theme='dark'] {
  --background: #0a0f1e;
  --foreground: #e7ebf4;
  --card: #10172b;
  --card-foreground: #e7ebf4;
  --popover: #131b31;
  --popover-foreground: #e7ebf4;
  --primary: #eef1f8;
  --primary-foreground: #0a0f1e;
  --secondary: #1a2340;
  --secondary-foreground: #dfe5f2;
  --muted: #151d34;
  --muted-foreground: #8f9ab3;
  --subtle-foreground: #66718b;
  --accent: #1a2340;
  --accent-foreground: #e7ebf4;
  --brand: #ec4f95;
  --brand-soft: #3a1530;
  --brand-ink: #f78bbb;
  --destructive: #f0606e;
  --destructive-soft: #3a1720;
  --success: #3cc994;
  --success-soft: #0f2e26;
  --warning: #f0b04d;
  --warning-soft: #33260f;
  --info: #7d9bf2;
  --info-soft: #17234a;
  --border: #1d2640;
  --border-strong: #2a3555;
  --input: #283252;
  --ring: #ec4f95;
  --prov-tag: #9fb0d6;
  --prov-tag-soft: #1a2440;
  --prov-path: #5cd0c5;
  --prov-path-soft: #0f2c2d;
  --prov-edit: #f78bbb;
  --prov-edit-soft: #3a1530;
  --prov-community: #b59cff;
  --prov-community-soft: #251b45;
  --chart-1: #e4458b;
  --chart-2: #6680ec;
  --chart-3: #17a093;
  --chart-4: #cc7f16;
  --chart-5: #8f72f2;
  --seq-0: #151d34;
  --seq-1: #3a1a37;
  --seq-2: #6b1f4f;
  --seq-3: #a12a6a;
  --seq-4: #d9418a;
  --seq-5: #f58bbb;
  --sidebar: #10172b;
  --sidebar-foreground: #e7ebf4;
  --sidebar-primary: #eef1f8;
  --sidebar-primary-foreground: #0a0f1e;
  --sidebar-accent: #1a2340;
  --sidebar-accent-foreground: #e7ebf4;
  --sidebar-border: #1d2640;
  --sidebar-ring: #ec4f95;
  --topbar: rgb(10 15 30 / 0.78);
  --overlay: rgb(3 6 14 / 0.62);
  /* --glow is set per cover to its accent colour: covers glow on deep ink */
  --shadow-cover:
    0 1px 1px rgb(0 0 0 / 0.4), 0 8px 16px -4px rgb(0 0 0 / 0.55),
    0 18px 40px -12px var(--glow, rgb(236 79 149 / 0.18));
  --shadow-cover-hover:
    0 2px 2px rgb(0 0 0 / 0.4), 0 14px 24px -6px rgb(0 0 0 / 0.6),
    0 26px 60px -12px var(--glow, rgb(236 79 149 / 0.32));
  --shadow-overlay: 0 30px 80px -10px rgb(0 0 0 / 0.7), 0 0 0 1px rgb(255 255 255 / 0.04);
}
/* No prefers-color-scheme duplicate: public/theme-init.js (an external script in <head>)
   resolves the stored preference - light, dark or system - to an explicit data-theme before
   first paint, and src/lib/theme.ts follows OS changes while the preference is "system". */

@theme inline {
  --color-background: var(--background);
  --color-foreground: var(--foreground);
  --color-card: var(--card);
  --color-card-foreground: var(--card-foreground);
  --color-popover: var(--popover);
  --color-popover-foreground: var(--popover-foreground);
  --color-primary: var(--primary);
  --color-primary-foreground: var(--primary-foreground);
  --color-secondary: var(--secondary);
  --color-secondary-foreground: var(--secondary-foreground);
  --color-muted: var(--muted);
  --color-muted-foreground: var(--muted-foreground);
  --color-subtle-foreground: var(--subtle-foreground);
  --color-accent: var(--accent);
  --color-accent-foreground: var(--accent-foreground);
  --color-destructive: var(--destructive);
  --color-destructive-soft: var(--destructive-soft);
  --color-border: var(--border);
  --color-border-strong: var(--border-strong);
  --color-input: var(--input);
  --color-ring: var(--ring);
  --color-brand: var(--brand);
  --color-brand-soft: var(--brand-soft);
  --color-brand-ink: var(--brand-ink);
  --color-success: var(--success);
  --color-success-soft: var(--success-soft);
  --color-warning: var(--warning);
  --color-warning-soft: var(--warning-soft);
  --color-info: var(--info);
  --color-info-soft: var(--info-soft);
  --color-prov-tag: var(--prov-tag);
  --color-prov-tag-soft: var(--prov-tag-soft);
  --color-prov-path: var(--prov-path);
  --color-prov-path-soft: var(--prov-path-soft);
  --color-prov-edit: var(--prov-edit);
  --color-prov-edit-soft: var(--prov-edit-soft);
  --color-prov-community: var(--prov-community);
  --color-prov-community-soft: var(--prov-community-soft);
  --color-chart-1: var(--chart-1);
  --color-chart-2: var(--chart-2);
  --color-chart-3: var(--chart-3);
  --color-chart-4: var(--chart-4);
  --color-chart-5: var(--chart-5);
  --color-seq-0: var(--seq-0);
  --color-seq-1: var(--seq-1);
  --color-seq-2: var(--seq-2);
  --color-seq-3: var(--seq-3);
  --color-seq-4: var(--seq-4);
  --color-seq-5: var(--seq-5);
  --color-sidebar: var(--sidebar);
  --color-sidebar-foreground: var(--sidebar-foreground);
  --color-sidebar-primary: var(--sidebar-primary);
  --color-sidebar-primary-foreground: var(--sidebar-primary-foreground);
  --color-sidebar-accent: var(--sidebar-accent);
  --color-sidebar-accent-foreground: var(--sidebar-accent-foreground);
  --color-sidebar-border: var(--sidebar-border);
  --color-sidebar-ring: var(--sidebar-ring);
  --font-display: var(--font-display);
  --font-sans: var(--font-sans);
  --font-mono: var(--font-mono);
  --radius-sm: calc(var(--radius) - 6px); /* 8px  */
  --radius-md: calc(var(--radius) - 4px); /* 10px: buttons, inputs */
  --radius-lg: var(--radius); /* 14px: menus, toasts */
  --radius-xl: calc(var(--radius) + 2px); /* 16px: cards */
  --radius-2xl: calc(var(--radius) + 6px); /* 20px: dialogs, palette */
  --shadow-cover: var(--shadow-cover);
  --shadow-overlay: var(--shadow-overlay);
  --ease-out: var(--ease-out);
  --ease-spring: var(--ease-spring);
}
```

### Colour usage

| Token                                | Light     | Dark      | Use for                                                                                        |
| ------------------------------------ | --------- | --------- | ---------------------------------------------------------------------------------------------- |
| `--background`                       | `#f5f7fa` | `#0a0f1e` | Page canvas. Never cream or beige.                                                             |
| `--foreground`                       | `#121c36` | `#e7ebf4` | Body text (ink-navy, never pure black)                                                         |
| `--card`                             | `#ffffff` | `#10172b` | Cards, tables, inputs                                                                          |
| `--primary`                          | `#15203d` | `#eef1f8` | Primary buttons, toasts, bulk bar, save bar                                                    |
| `--brand`                            | `#db2777` | `#ec4f95` | Selection, progress, focus ring, one highlight per view                                        |
| `--brand-ink`                        | `#a3195a` | `#f78bbb` | Pink _text_ (links, chips)                                                                     |
| `--muted-foreground`                 | `#5b6680` | `#8f9ab3` | Secondary text                                                                                 |
| `--destructive`                      | `#c42b3c` | `#f0606e` | Delete, Danger zone. Always with a word, never colour alone                                    |
| `--success` / `--warning` / `--info` |           |           | Status only (healthy/direct, transcode/inactive/expiring, neutral notices). Never chart series |

Rules: status colours always come with an icon and a label. Text never takes a chart colour; a coloured
mark next to the text carries identity. Avoid terracotta/orange-brown UI accents.

### Charts

- Categorical `--chart-1..5` in that fixed order (validated: CVD ΔE ≥ 12 adjacent, both themes). A sixth
  series folds into "Other".
- Magnitude/heatmaps use `--seq-0..5` (one hue). Never a rainbow.
- One y-axis only. 2px lines, bars ≤18px wide with 4px rounded ends, dashed recessive gridlines,
  axis labels that name real values (`10h`, `2,500`). Every chart has a hover tooltip (`--primary` pill).
- Recharts via shadcn `ChartContainer`, but put the generated colour CSS in `globals.css` (CSP forbids
  the injected `<style>`).

---

## 4. Typography

| Role                    | Family              | Size / line-height  | Weight         | Tracking | Example                         |
| ----------------------- | ------------------- | ------------------- | -------------- | -------- | ------------------------------- |
| Display XL (book hero)  | Bricolage Grotesque | 52 / 52 (mobile 32) | 750            | -0.035em | The Way of Kings                |
| Display (page title)    | Bricolage Grotesque | 34 / 36 (mobile 27) | 700            | -0.03em  | Good evening, Chris.            |
| Heading (section)       | Bricolage Grotesque | 19 / 22             | 680            | -0.02em  | Recently added                  |
| Title (card)            | Bricolage Grotesque | 15 / 18             | 650            | -0.01em  | Listeners                       |
| Stat value              | Bricolage Grotesque | 30 / 30 (tnum)      | 700            | -0.03em  | 61.4                            |
| Body                    | Figtree             | 14 / 21             | 400            | 0        | Edits are saved as overrides... |
| Label                   | Figtree             | 13 / 18             | 600            | 0        | Folder naming template          |
| Caption                 | Figtree             | 12.5 / 18           | 400, muted     | 0        | 62% · chapter 21 · 2 min ago    |
| Eyebrow                 | Figtree             | 12 / 16             | 600, uppercase | +0.06em  | THE STORMLIGHT ARCHIVE · BOOK 1 |
| Mono (paths, ids, logs) | JetBrains Mono      | 12.5 / 20           | 400–600        | -0.01em  | /mnt/tank/audiobooks/fiction    |

- All numbers use `font-variant-numeric: tabular-nums` (`tabular-nums` utility).
- Do not use Inter or Space Grotesk.
- Cover art fonts (Anton, Cinzel, Fraunces, Instrument Serif, Fredoka) appear **only inside procedural
  covers**, never in UI chrome. Self-host them with fontsource like the UI fonts.
- Text never baked into images. Every label must survive +30% length (de/fr/pt/es/it): no fixed-width
  buttons, labels wrap, segmented controls scroll horizontally.

---

## 5. Space, radius, elevation, density

- **Spacing** (4px grid): 4 icon gap · 8 inline gap · 12 row gap · 16 card gap · 20 card padding ·
  24 page gutter (16 on mobile) · 32–40 between sections/shelves.
- **Radius**: covers 5px (7px in the hero; physical objects stay crisp) · 8 small controls ·
  10 buttons/inputs · 14 menus/toasts (`--radius`) · 16 cards · 20 dialogs/palette · full for pills/avatars.
- **Elevation**: three levels only.
  1. Flat + 1px `--border` hairline: everything (cards, tables, inputs). Cards never cast shadows.
  2. `--shadow-cover`: covers only (lift to `--shadow-cover-hover` + translateY(-3px) on hover).
  3. `--shadow-overlay`: menus, dialogs, sheets, toasts, palette, bulk/save bars.
- **Density**: comfortable by default. Field rows 48px, buttons 36px (sm 30, lg 44), cover grid
  min 158px columns with 28/22px gaps, table rows ~44px. Touch targets ≥44px on mobile.

---

## 6. Motion

| Token                              | Value                          | Use                                                                                 |
| ---------------------------------- | ------------------------------ | ----------------------------------------------------------------------------------- |
| `--dur-1`                          | 120ms                          | Hover, press, colour                                                                |
| `--dur-2`                          | 200ms                          | Dialog/menu entrance, cover lift                                                    |
| `--dur-3`                          | 320ms                          | Sheets, toasts, bulk bar                                                            |
| `--ease-out`                       | `cubic-bezier(.2,.8,.2,1)`     | Default entrance                                                                    |
| `--ease-spring`                    | `cubic-bezier(.34,1.36,.64,1)` | Switch thumb, toast/bulk bar (small overshoot)                                      |
| `view-transition-name: hero-cover` | 420ms                          | Shared-element: grid cover grows into the book hero and back (View Transitions API) |

Animate opacity, transform and colour only. Never animate chart values on load, layout sizes or text.
Only live indicators loop (pulse on "listening now"). `prefers-reduced-motion: reduce` collapses every
animation/transition to ~0 and skips the view transition.

---

## 7. Iconography

lucide-react, 24px grid, 2px stroke, round caps/joins. Sizes: 15 (inline/meta), 16 (buttons), 18
(nav), 20 (dialog badges). Icons label, they don't decorate: always next to text or with `aria-label`.
Fixed meanings: `tag` file tag · `folder` path · `lock` edited/locked · `globe` community ·
`transcode` (repeat) transcoding · `move` rename on disk · `split` folder override · `unplug` root
unavailable · `shield` safety · `sparkles` match/enrich. No emoji anywhere.

---

## 8. Components

All names are shadcn/ui components unless marked _custom_.

### Button

Variants: `default` (ink), `brand` (pink; only for the one key confirm on a view, e.g. "Review and save"),
`outline`, `secondary`, `ghost`, `destructive`, `destructive-outline`, `link`. Sizes `sm` 30 / `default` 36 /
`lg` 44 / `icon`. Labels are verb + object + count: "Rename 12 folders", "Accept 4 fields". Loading state
keeps the label and spins the icon ("Scanning..."). Disabled = 45% opacity, no pointer.

### Input, InputGroup, Textarea, Select, Combobox, Field

38px, radius 10, `--input` border, focus = `--ring` border + 3px 20% ring. Errors: `aria-invalid`, red
border, message under the field that says how to fix it. Paths and ids use the mono font.

### Switch, Checkbox, RadioGroup (as radio cards)

Switch on = `--brand`. Checkbox checked/indeterminate = `--brand`. Choices with consequences use
**radio cards** (title + one-line description), e.g. folder detection, HTTPS mode, access level.

### Badge / status

Pills 22px: `secondary`, `outline`, `ink` (Admin), `success`, `warning`, `destructive`, `info`, `brand`.
Status = dot + label (`Direct`, `Transcode`, `Listening now` with pulse).

### Tabs / segmented control

Sub bar and in-page section switches use the **segmented** Tabs variant (muted track, raised white
active). Detail pages (user) use **underline** Tabs. Counts sit in the trigger in subtle tabular text.

### Card, Table (TanStack Table + Virtual)

Cards: radius 16, hairline, header row 16/20 padding with title + muted description. Tables live in a
bordered rounded container that scrolls on its own; header 12px muted; row hover = muted 70%; selected
row = brand 7%. On mobile tables become stacked rows (primary cell + status; the rest hidden or wrapped).

### Dialog / Sheet (Drawer) / Popover / DropdownMenu

Dialog: radius 20, 40px tinted icon badge (brand/danger/info/success/community), sticky footer; on mobile
it becomes a bottom sheet. Sheet: right side, 420px, used for faceted filters. Menus: radius 14, 6px padding,
items 36px with a 16px muted icon; destructive items are red and separated by a divider.

### Toast (Base UI Toast via shadcn; never sonner, which injects <style> and breaks the CSP)

Ink (`--primary`) background, 22px icon tile, title + one-line description, one optional action
("Undo"). Bottom-right (above the tab bar on mobile). Every reversible destructive action toasts with Undo.

### Command palette (cmdk) _signature_

Opened from the top-bar omnisearch, ⌘K / Ctrl+K or `/`. It opens over the omnisearch so it reads as
the field expanding. Groups: Actions, Books (cover thumb), People (avatar), Authors, Series, Narrators,
Shares, Settings, Go to. Items are 48px with a 36px leading visual, title with the match in `--brand-ink`
bold, muted subtitle; `↵` hint on the active row; footer with key hints and result count.

### Cover _custom, signature_

Square (audiobook covers are square; art that isn't is shown whole over a blurred copy of itself), radius 5, `--shadow-cover`, a faint spine crease + gloss overlay
and a noise texture. Real art when present. Otherwise a **procedural cover**: deterministic from
`title|author`; layout by genre (sci-fi planet, epic gold-frame sigil, literary colour field + serif
italic, crime black band + condensed type, non-fiction bold grotesk + rule, kids hills + rounded type,
comic halftone + starburst, memoir duotone sun, classics tri-band); palette by hash; typography scales
with container query units (`cqw`) so one component works from 30px thumbnails to the 300px hero.
Missing state: diagonal hatch, `image-off` icon, title. In the real build render the art as a React SVG
component (not an innerHTML string) so it stays CSP-clean; set `--c1..--c3`, `--ink`, `--glow` via the
`style` prop (CSSOM, allowed under `style-src 'self'`).

### Cover tile, shelf row, cover grid _custom_

Tile = cover + 2-line title + muted author/series (the author links to their books). Hover lifts
the cover; a checkbox appears top-left (always visible once anything is selected); up to two flag
chips top-right (no cover, unmatched, transcode, suspect, duplicate). Selected = cover scales to 94% inside a 2.5px pink ring.
Shelf rows scroll horizontally with snap; the grid is `repeat(auto-fill, minmax(158px, 1fr))`
(2 columns on mobile), virtualized in production.

### Bulk action bar / Save bar _custom (fixed Toolbar)_

Floating ink pill, bottom-centre: "N selected" + ghost actions (Edit fields, Match, Add to share, Rename
on disk, More) + clear. The save bar appears when a book has unsaved edits: "3 unsaved changes ·
Discard · Review and save" → a diff dialog (old struck through in red, new in green, provenance
before → Edited).

### Provenance marker + Field row _custom, signature_

`[icon] File tag | Path | Edited | Community` in a 20px soft pill (`--prov-*` tokens); `Unsaved` is a
dashed pink outline. Field row = label (130px) · click-to-edit value · marker (+ "Revert" for edited
fields, whose tooltip shows the file-tag value). Editing locks the field against rescans; the legend
under the Details card explains the four sources.

### Chapter ribbon _custom_

Proportional timeline: one segment per chapter (flex = duration, 2px gaps), coloured per file with
`--chart-n` (alternating chapters at 86%/62% mix), a file strip underneath for multi-file books, axis
with total time; hover syncs with the chapter list. List rows: number · title (click to rename) · start ·
length.

### Series spines _custom, signature_

Owned books as vertical spines (cover palette colour, bands, rotated title, index), missing entries as
dashed ghost spines with their number and title from community series data; header says "You have 1, 2,
4 of 5; missing 3, 5". Spines lift on hover and open the book.

### Avatar + progress ring _custom on shadcn Avatar_

Gradient monogram (two hues per user), Bricolage initials. Ring: `--border` track, `--brand` progress,
`--success` when finished. Avatar stacks overlap by 8px with a 2px `--card` outline.

### Stat tile with sparkline

Label (12.5 muted) · value (Bricolage 30, tnum) + 96×34 sparkline (2px line, 10% area fill, end dot) ·
delta (green up / red down with arrow) + muted context. Text stays ink; only the line takes a chart colour.

### Notice (Alert)

Icon tile + bold one-line headline + muted explanation + at most two actions. Tones: `bad` (root
unavailable), `warn`, `info`, `safe` (green shield: "Safety stop: nothing was deleted").

### Invite card _custom_

Deep ink card with pink/plum radial glows, three fanned covers from the share, server mark, a QR code,
"Uncle Ray, your audiobooks are waiting." Below: read-only mono link + Copy, uses/expiry, "code rides in
the link fragment, never in server logs".

### Danger zone

Separate bordered block at the bottom of a page (red header), one row per irreversible action, with the
reversible alternative offered first ("Disable account instead"). Delete requires type-to-confirm (the
handle). Never place a destructive button next to Save.

### Empty state, Skeleton

Empty: icon or hatch covers, one-line headline, one sentence, one action. Skeleton: square cover
placeholders exactly in the grid slot (no layout shift), shimmer 1.4s (off with reduced motion).

---

## 9. Patterns

- **Edit metadata** (default, non-destructive): click value → edit → row turns pink-tinted with
  `Unsaved` → save bar → diff dialog → saved as path-keyed override, marker becomes `Edited` (locked).
  "Revert" restores the file tag (with Undo toast).
- **Match with community**: candidates as radio cards (cover, edition, narrator, length vs. your files,
  match %), then a side-by-side table: _On your server_ (with provenance) vs _Community_, a checkbox per
  differing field; your own edits are unticked by default. The cover is the table's first row (the
  book's art beside the community's, fetched by the server as a thumbnail, since the CSP loads no
  other host's images), ticked only for a book without art. "Accept N fields".
- **Rename on disk** (gated): always shows the template and a before → after path diff per book
  (changed segments struck/inserted). When "Allow renaming and moving folders" is off the dialog is a
  labelled dry run with a disabled primary and a link to Server › Files. Always states that progress
  follows via move-tracking.
- **Folder detection**: radio cards Automatic / Always one book / Separate books, with what each means
  _for this folder_ ("Here: 3 books"). A folder whose audio is only in its disc folders (CD1, CD2...:
  the server's `split_discs`) offers Automatic and Always one book (which joins them, in disc order);
  Separate books stays disabled there. Any other folder without audio of its own (an author's, a
  series') offers neither, and the detection dialog doesn't offer Always one book on it: the server
  would read it the same.
- **Safety stops**: unavailable roots are shown with the shield, the count of preserved books and
  listeners, and the fix ("Mount /mnt/nas/lectures and retry").
- **Health triage**: category cards (count, icon, fanned covers) → queue with cover, reason, path, one
  fix button + Ignore (Undo), bulk select. Duplicates use a side-by-side compare.

---

## 10. Voice and copy

- Plain, specific, active. Buttons say what happens: "Rescan library", "Send invite", "Rename 12 folders".
- Errors say what went wrong and how to fix it: "/mnt/nas/lectures isn't reachable. Mount the NAS, then retry."
- Say whether anything was lost. Usually nothing was, and saying so is the point.
- Name things the way an admin thinks: "Who can see this", "What can Sam listen to?", never `share_paths`.
- Sentence case. No emoji, no exclamation marks. Numbers with thousands separators and tabular figures.
- Relative times for recency ("14 min ago"), absolute for records ("Today 03:00").

---

## 11. Do and don't

| Do                                             | Don't                                                  |
| ---------------------------------------------- | ------------------------------------------------------ |
| "Rename 12 folders" in an ink button           | "Submit" in a pink button                              |
| Shadows on covers only; flat hairline cards    | Floating cards with heavy shadows                      |
| Destructive actions in a separate Danger zone  | Delete next to Save                                    |
| Show provenance on every editable field        | Silently overwrite tags on rescan                      |
| Celebrate safety stops ("Nothing was deleted") | Show an unmounted share as a red error with no context |
| One pink highlight per view                    | Pink buttons, pink headings and pink charts together   |
| Status = dot/icon + word                       | Colour alone                                           |
| Covers in every list of books                  | Text-only book tables as the default view              |

---

## 12. Accessibility

- Visible focus: 2px `--ring` outline, 2px offset, on every interactive element.
- Palette, menus, tabs and dialogs are keyboard operable (arrows, Enter, Esc); dialogs are
  `aria-modal`, labelled by their title; the palette is a `combobox` + `listbox`.
- Contrast: body and muted text meet WCAG AA on both themes; pink text uses `--brand-ink`.
- Charts have `role="img"` + `aria-label`, hover tooltips, and a table/list alternative nearby
  (top books, apps in use are lists, not only bars).
- `prefers-reduced-motion` respected everywhere, including the shared-element cover transition.
- Mobile: 16px gutters, no horizontal page scroll; wide content scrolls in its own container.
  A grid that only gets columns from a breakpoint (`md:grid-cols-2`) starts from `grid-cols-1`
  (`minmax(0, 1fr)`): without it the phone layout is one implicit `auto` column, which grows to
  the full width of any `truncate`d (nowrap) line inside it, so `min-w-0` never gets to shrink it.

---

## 13. CSP rules (the console is served with `script-src 'self'; style-src 'self'`, no nonce)

The policy is the server's `web.contentSecurityPolicy` and does not change for the console. So:

- **No inline `<script>` or `<style>`, no `style=""` or `on*=""` attributes in HTML.** `index.html`
  loads `theme-init.js` and the Vite bundle as files. `scripts/check-csp.mjs` fails `npm run build`
  otherwise, and `internal/web/adminui` tests the embedded build the same way.
- **Inline styles only through React's `style` prop** (React writes them through CSSOM, which the
  CSP allows). Per-item colours (`--c1`, `--glow`, bar widths, avatar gradients) go there.
- **`<CSPProvider disableStyleElements>`** wraps the app, so Base UI never renders its `<style>`;
  the one rule it needs (`.base-ui-disable-scrollbar`) lives in `globals.css`.
- **Banned (they inject styles or scripts):** Radix (`@radix-ui/*`), sonner, vaul, next-themes,
  ECharts, cmdk's `<Command.Dialog>` (wrap `<Command>` in Base UI Dialog instead), shadcn's
  `ChartStyle` (put chart colours in `globals.css`), `<style>` JSX and `dangerouslySetInnerHTML`.
  ESLint enforces all of these (`eslint.config.js`), and `src/app.test.tsx` asserts no `<style>`
  element exists while the palette, a menu and a toast are open.
- **Build assets are files, never `data:` URIs** (`build.assetsInlineLimit: 0`; no `?inline` CSS). Fonts
  are self-hosted with fontsource; Google Fonts is a cross-origin request the CSP blocks.
- **Images:** covers are fetched as thumbnails from `POST /api/v1/admin/covers` with the
  `Authorization` header and rendered as the `data:` URLs it returns (`useCover`). Never put the
  session token in an image URL (`?token=`): this is a full-privilege admin credential and URLs
  leak into proxy logs and history. `img-src` allows `'self' data:` but **not** `blob:`.
- The Vite dev server (`npm run dev`) is not under this CSP, so a violation can hide in dev:
  check the production build (`npm run build`, then the Go server) before calling UI work done.
