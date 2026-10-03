# admin-ui: the AudioSilo admin console

The admin console the server serves at `/admin`: a React 19 + Vite + TypeScript single-page app
built with shadcn/ui on Base UI and Tailwind v4, in the **Shelf** design
([STYLEGUIDE.md](STYLEGUIDE.md), authoritative). It is a static client over the server's JSON API;
the API enforces the admin role, so nothing here is privileged.

`npm run build` writes to `../internal/web/adminui/dist`, which the Go binary embeds. That output
is **not committed**: CI, the Dockerfile and GoReleaser build it. A Go build without it compiles
and serves a "console not built" page at `/admin`.

During the redesign the new console is behind a switch: `AUDIOSILO_ADMIN_NEXT=1` serves it at
`/admin` and moves the classic console to `/admin/classic`. Without the switch `/admin` is the
classic console. (The switch and the classic console go away at the cutover, Phase 1b of
`ADMIN-CONSOLE-PLAN.md` in the workspace.)

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
# terminal 1: the server, plain HTTP, with the new console switched on
AUDIOSILO_TLS_MODE=off AUDIOSILO_ADMIN_NEXT=1 go run ./cmd/audiosilo --data ./data
# terminal 2: hot-reloading console
npm --prefix admin-ui run dev   # open http://localhost:5173/admin/
```

Set `AUDIOSILO_DEV_SERVER` if the Go server isn't on `127.0.0.1:8080`. The dev server is **not**
under the production CSP, so check CSP-sensitive changes against a real build served by Go.

## Layout

```
src/api/          hand-mirrored wire types, fetch client (bearer + 401 handling), TanStack Query hooks
src/components/   shell (top bar, sub bar, tab bar, ⌘K palette), shadcn/ui primitives (ui/), shared bits
src/features/     one folder per screen (overview, auth, coming-soon, ...)
src/i18n/         i18next setup + locales/<lang>.json (en is the base; i18n.test.ts keeps them in step)
src/lib/          session, theme, toast, formatting - logic kept out of components so it's testable
src/styles/       globals.css: the Shelf tokens and the few signature classes
public/           theme-init.js (applies the theme before first paint; external because of the CSP)
scripts/          check-csp.mjs (fails the build on inline script/style)
```

Routing is TanStack Router (code-based routes in `src/router.tsx`, `basepath: '/admin'`), chosen
over React Router for typed params and search params (later screens keep filters in the URL) and
to share conventions with TanStack Query and Table.

## Adding shadcn/ui components

`components.json` is set up for the Base UI base (`base-nova`), so `npx shadcn@latest add <name>`
works, with two caveats: the current CLI rewrites the `cn` import to `from "cn"` and installs an
unrelated npm package of that name, so fix the import to `@/lib/utils` and `npm rm cn` after
adding; and restyle what you add to the Shelf tokens (see the existing `src/components/ui/`
files). Only keep components something uses.
