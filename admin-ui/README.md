# admin-ui: the AudioSilo admin console

The admin console the server serves at `/admin`: a React 19 + Vite + TypeScript single-page app
built with shadcn/ui on Base UI and Tailwind v4, in the **Shelf** design
([STYLEGUIDE.md](STYLEGUIDE.md), authoritative). It is a static client over the server's JSON API;
the API enforces the admin role, so nothing here is privileged.

`npm run build` writes to `../internal/web/adminui/dist`, which the Go binary embeds. That output
is **not committed**: CI, the Dockerfile and GoReleaser build it. A Go build without it compiles
and serves a "console not built" page at `/admin`.

It replaced the classic vanilla-JS console at the Phase 1b cutover (`ADMIN-CONSOLE-PLAN.md` in
the workspace tracks the phases still to come).

## Commands (Node 24, see `.nvmrc`)

```sh
npm ci
npm run dev       # Vite on :5173, proxies /api etc. to the Go server on :8080
npm run check     # typecheck + eslint + prettier --check + vitest (part of the server gate)
npm run build     # production build into ../internal/web/adminui/dist + the CSP check
```

From the repo root, `scripts/build-admin.sh` runs `ci` + `check` + `build`.

## Dev loop

```sh
# terminal 1: the server, plain HTTP
AUDIOSILO_TLS_MODE=off go run ./cmd/audiosilo --data ./data
# terminal 2: hot-reloading console
npm --prefix admin-ui run dev   # open http://localhost:5173/admin/
```

Set `AUDIOSILO_DEV_SERVER` if the Go server isn't on `127.0.0.1:8080`. The dev server is **not**
under the production CSP, so check CSP-sensitive changes against a real build served by Go.

## Layout

```
src/api/          hand-mirrored wire types, fetch client (bearer + 401 handling), TanStack Query hooks
src/components/   shell (top bar, sub bar, tab bar, ⌘K palette), shadcn/ui primitives (ui/), shared bits
src/features/     one folder per screen (overview, library/*, book, libraries, people, ...); each
                  feature's pure logic sits in a *-model.ts with its own test, and section-page.tsx
                  lazy-loads each screen as its own chunk
src/i18n/         i18next setup + locales/<lang>.json (en is the base; i18n.test.ts keeps them in step)
src/lib/          session, theme, toast, formatting - logic kept out of components so it's testable
src/styles/       globals.css: the Shelf tokens and the few signature classes
public/           theme-init.js (applies the theme before first paint; external because of the CSP)
scripts/          check-csp.mjs (fails the build on inline script/style)
```

Routing is TanStack Router (code-based routes in `src/router.tsx`, `basepath: '/admin'`), chosen
over React Router for typed params and search params (later screens keep filters in the URL) and
to share conventions with TanStack Query and Table.

Covers come from `POST /api/v1/admin/covers`: `useCover(libraryId, path, size)` queues each
cover and `src/api/cover-batch.ts` sends everything asked for in the same moment as one request
of up to 60 thumbnails (data: URLs, the CSP allows no blob:), so a grid of hundreds of covers is a
handful of requests (160px for rows, 320px for tiles, 640px for the book hero, whose blurred
backdrop and tint use the 160px one). A thumbnail nobody shows any more by the time its batch goes
out isn't asked for, and unused ones leave the cache after five minutes. A book without art gets a generated cover (`src/components/generated-cover.tsx`, React SVG, palette from
`src/lib/cover-model.ts`). Book pages are addressed by identity, `/admin/library/book?library=&path=`
(`src/lib/book-route.ts`), never by an internal id.

Forms use react-hook-form, with zod schemas where a field has rules; schema messages are i18n
keys (`fieldMessage` in `src/lib/errors.ts` translates them, and passes server errors through).
Dialogs compose `DialogContent` + `DialogBody` + `DialogFooter` (`src/components/ui/dialog.tsx`);
a form wraps body and footer in `<form className="contents">`. Library reordering uses dnd-kit
(pointer and keyboard); QR codes are drawn as SVG paths with uqr, in the browser.

Charts (the Activity screens, a person's listening year) use Recharts through
`src/components/ui/chart.tsx`, shadcn's chart minus `ChartStyle`: series colours are CSS
variables passed as props (`fill="var(--chart-1)"`) and the chart CSS lives in `globals.css`
(`.chart`), because shadcn injects a `<style>` the CSP blocks. Recharts only writes styles through
the CSSOM. It loads as its own lazy chunk with the screens that draw charts. Heatmaps are hand-built
grids on the `--seq-*` scale (`src/features/activity/heatmaps.tsx`).

`@hookform/resolvers` is held at 5.2.x: 5.9 declares an optional `effect` peer that npm 11 fails
to resolve (ERESOLVE) on a fresh `npm install`.

## Adding shadcn/ui components

`components.json` is set up for the Base UI base (`base-nova`), so `npx shadcn@latest add <name>`
works, with two caveats: the current CLI rewrites the `cn` import to `from "cn"` and installs an
unrelated npm package of that name, so fix the import to `@/lib/utils` and `npm rm cn` after
adding; and restyle what you add to the Shelf tokens (see the existing `src/components/ui/`
files). Only keep components something uses.
