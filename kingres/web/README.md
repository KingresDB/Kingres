<!-- SPDX-License-Identifier: PostgreSQL -->

# KingresDB web panel

The management console of KingresDB: a single-page application (Vue 3, TypeScript, Vite) that is a client
of the REST API in `kingres/api/openapi.yaml` (revision 0.3.0, 86 operations) and nothing else. The Go server (`kingresd`) will embed the
production build from `dist/` and serve it on every node. The information architecture follows the
"Management panel" chapter of `docs/architecture.md`: a first-run wizard, a **Cluster** scope and a
**This server** scope.

## Quick start

```
npm ci
npm run dev          # http://localhost:5173 with the in-memory mock API (no backend needed)
```

Sign in with `admin`, `ops`, `viewer` or `metrics` and the password `demo-password`. The roles are
`kingres_admin`, `kingres_ops`, `kingres_readonly` and `kingres_stats`, so you can see how pages, buttons and navigation
change with the cluster role.

The mock cluster has three servers (chained placement: a primary on every server and a standby of the
neighbouring group), a map group, three shard groups and two databases: `shop` (sharded, 128 buckets, ledger
tables) and `reports` (plain). Mutations are tasks that progress for a few seconds and then change the state, so
adding a server, rebalancing, failing over or creating a database visibly changes the console.

Development switches in the URL (mock API only):

| Parameter                                      | Effect                                                                                                                                                        |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `?mock=uninitialized`                          | Starts as a fresh server and shows the first-run wizard. The setup token is `demo-setup-token`. A cluster named `failme` makes setup fail (to see the retry). |
| `?unsupported=createInclusionProof,listAlerts` | Those operations answer `unsupported_operation`, to see the "not available on this server version yet" state.                                                 |

Reload the page to reset the mock state. Creating a database named `failme` fails half way, to see a failed
task. The `metrics` account has only `kingres_stats`: it lands on the Dashboard with the charts it may read.
The third server and the `reports` database report some values as `null` (not available), which the panel
shows as "n/a" with an accessible label, never as 0.

### Against a real backend

```
npm run dev:backend  # mocks off; Vite proxies /api to http://127.0.0.1:8091
```

The proxy target is in `vite.config.ts`. `VITE_MOCKS=off` (see `.env.backend`) turns the mocks off. The mock layer
is imported only when `import.meta.env.DEV` is true, so it is not in the production build (checked: no `msw`
string in `dist/`).

## Scripts

| Script                        | What it does                                                                             |
| ----------------------------- | ---------------------------------------------------------------------------------------- |
| `npm run dev` / `dev:backend` | Dev server with the mock API / with the proxy to `127.0.0.1:8091`                        |
| `npm run build`               | Type-check, then build to `dist/` (the directory the Go server embeds)                   |
| `npm run typecheck`           | `vue-tsc --noEmit` (scripts and templates, `strict`, `noUncheckedIndexedAccess`)         |
| `npm run lint`                | ESLint (flat config, Vue and Vue accessibility plugins) and `prettier --check`           |
| `npm run lint:fix` / `format` | Fix lint problems / format with Prettier                                                 |
| `npm test`                    | Vitest and Testing Library, against the mock API                                         |
| `npm run api:generate`        | Regenerate the API types from the contract                                               |
| `npm run api:check`           | Fail if the generated files are out of date (also run by `npm test`)                     |
| `npm run brand:sync`          | Copy the favicons from `kingres/brand/` into `public/` and regenerate the logo path data |
| `npm run brand:check`         | Fail if the committed brand files differ from `kingres/brand/` (also run by `npm test`)  |

## End-to-end test against the real server

`npm run e2e` is one command. It needs Windows with WSL2 (distribution `Ubuntu-24.04`, user `kingres`), Go, Chrome or Edge, and
the installed KingresDB at `~/kingres-work/install` inside WSL. It:

1. builds `kingresd` for Linux from `kingres/control-plane` (or uses `KINGRESD_BIN`),
2. copies the installation to `~/kw-web/install`, starts `kingresd` there on a fresh data directory (API port 58201,
   PostgreSQL port 58200; override with `E2E_API_PORT`, `E2E_PG_PORT`),
3. starts the panel with the mock off (`vite --mode backend`, proxy to the backend, port 5188),
4. drives a headless browser through the real flow (`scripts/e2e/flow.mjs`): wizard with the real setup token (wrong
   token and wrong data directory first), sign-in, reload, roles (duplicate, disable, a stats-only account), a sharded
   database with the ledger, tables made with psql, distribute and enable-ledger from the panel, workbench query and
   SQL error, grant, network rule, every page, calm notices for unsupported operations, the audit log, drop database,
   a server restart that signs everyone out, and the stats-only and read-only accounts,
5. prints a report (notes, per-page state, every HTTP error the browser saw) and tears everything down.

The exit code is 0 only if every step passed. Screenshots are in `.e2e/shots/` (ignored by git). `E2E_KEEP_INSTALL=1`
keeps `~/kw-web/install` for faster re-runs; `E2E_KEEP_RUNNING=1` leaves the processes running. `scripts/e2e/backend.sh` is
the WSL side (`prepare`, `start`, `restart`, `token`, `psql`, `stop`). `puppeteer-core` is a development dependency.

### Where kingresd 0.2.0-dev differs from the contract (found by the run)

The panel copes with these; they are for the server team to fix. Only the first one needs a workaround in the panel (the
Ledger page falls back to per-database status, see `src/api/queries.ts`).

- `GET /ledger/status` answers 404 `not_found` (the contract lists 200).
- `GET /grants` without `grantee` and `database` answers 404 (the panel always sends both).
- A table with a ledger has `classes: [plain, ledger]`; the contract says `plain` never combines.
- `Server.isManagerLeader` is false while `GET /this-server` says `isManagerLeader: true` and `ClusterOverview.managerLeader` is null.
- `ClusterSummary.databaseCount` counts 3 databases where `GET /databases` lists 2.

## Brand

The logos are the files the project owner supplied in `kingres/brand/` (read its `README.md` for variants, colorings
and rules). They are the source of truth and are not redrawn:

- `BrandLogo` (`src/components/BrandLogo.vue`) draws the logo as inline SVG with the exact path data from the SVG
  files. Props: `variant` (`mark`, `horizontal`, `stacked`), `size` (height in px) and `decorative`. The crown and
  "KINGRES" use `--logo-crown`, "DB" uses `--logo-db`, so the light theme shows the `color-light` coloring and the
  dark theme `color-dark`, with no background; the shield always has the gold gradient. Gradient and clip-path ids are
  unique per instance. The logo is named "KingresDB"; use `decorative` when the name is written next to it.
- `npm run brand:sync` copies `public/favicon.svg`, `favicon-32.png`, `apple-touch-icon.png` and `icon-512.png` from
  `kingres/brand/` and writes `src/components/brand.generated.ts` (the path data). The results are committed;
  `tests/brand-files.test.ts` fails when they differ from the source (run the script again).
- `public/manifest.webmanifest` and the `<head>` of `index.html` give the name, the icons and a `theme-color` for light
  and dark.
- Where it appears: the top bar (horizontal lockup, the mark alone below 860 px, links to the dashboard), the sign-in
  page (stacked), every step of the first-run wizard (horizontal), the splash screen while the application starts,
  and the not-found and not-allowed pages (mark).
- Brand colours are tokens in `tokens.css` (`--brand-*`). The gold is a decoration and logo colour only: the top bar
  strip, the sign-in card strip and the active navigation mark (`--c-brand`, a darker gold on light surfaces). It is never
  a text colour and never replaces the semantic status colours; tests check both.

## API types

`src/api/schema.d.ts` is generated by `openapi-typescript` from `kingres/api/openapi.yaml`, and
`src/api/operations.generated.ts` (operation id to `x-required-role`, method and path) by the same script
(`scripts/gen-api.mjs`). Both are committed and never edited by hand. `tests/contract.test.ts` regenerates them in
memory and fails when the committed files differ from the spec, so after changing the contract run
`npm run api:generate` and commit the result. The script also writes the closed list of audit actions. The same test checks that the mock API has a handler for every
one of the operations (the count is read from the spec).

## Project structure

```
src/
  api/          client.ts (typed fetch, CSRF and setup-token headers, unwrap), endpoints.ts (one function per
                operation), queries.ts (TanStack Query hooks and keys), problem.ts (Problem -> message, keyed
                on `code`), roles.ts (role checks from the contract), generated files
  stores/       Pinia: session, theme
  composables/  useTask.ts: follow a 202 + Task by polling /tasks/{id}; useTaskAction wraps start + follow + refresh
  router/       routes (lazy), guard: setup state -> session -> sign-in -> role
  layouts/      ShellLayout: top bar, navigation, banners
  components/   AppDialog, TaskActionDialog, ConfirmDialog, GateButton, QueryBoundary, ProblemNotice,
                TaskProgress, StatusBadge, LineChart, BucketGrid, TabBar, FormField, ... and security/
  pages/        one file per route
  lib/          format.ts, status.ts (status -> tone, icon, label), validators.ts, cluster.ts, metrics.ts
  styles/       tokens.css (the only place for colours, sizes, fonts), base.css (shared classes)
  mocks/        the in-memory cluster (state.ts), request handlers (handlers.ts, http.ts), browser and node setup
  bootstrap.ts  builds pinia + query client + router (used by main.ts and by the tests)
tests/          Vitest; helpers.ts renders the real app against the mock API
scripts/        gen-api.mjs
```

## Conventions

- **Server state** only through TanStack Query (`src/api/queries.ts`); components never call `fetch`. Pinia
  holds only the session and the theme. Polling: cluster-wide status every 3 s, lists every 10 s; background
  tabs are not polled.
- **Errors** are `ApiError` (RFC 9457 `Problem`). `describeProblem` is the one place that maps `code` to text,
  tone and retry advice; `Record<ProblemCode, ...>` makes the compiler fail if the contract adds a code without
  a message. `unsupported_operation` is shown as calm information ("Not available on this server version yet").
- **Roles:** the role an operation needs comes from `x-required-role`. Navigation items are hidden when the role is
  too low (route `meta.operation`), actions use `GateButton op="..."`, which stays visible but `aria-disabled`
  with the reason. The backend enforces the same rules; the UI is a convenience, not a security boundary.
- **Setup** never uses `GET /tasks/{id}` (it needs a session): the wizard checks the token with
  `POST /setup/token-check` and follows `setupTask` in `GET /setup/state`.
- **`null` means "not available":** use `NullableValue` / `NotAvailable` (shows "n/a", read as "Not available"), never
  a formatter that turns null into 0.
- **Tasks:** long operations return 202 + `Task`. `TaskActionDialog` states the consequence, asks for confirmation,
  follows the task with `useTask` (progress bar, steps, `aria-live` summary) and invalidates the affected queries.
- **Accessibility:** semantic HTML, every control has a label (`FormField`), dialogs trap focus and close on
  Escape, status is icon plus text, `aria-live` for progress and errors, visible focus ring, tabular numerals.
  Contrast of the token pairs is tested in both themes.
- **Design tokens:** use `var(--...)` from `tokens.css`; do not write colours or font sizes in components.
- **Text** is English and kept next to the feature that shows it. Every source file starts with an
  `SPDX-License-Identifier: PostgreSQL` comment (checked by a test).

## Adding a page

1. Add the operation(s) it needs to `src/api/endpoints.ts` and a hook (with a key) in `src/api/queries.ts`.
2. Create `src/pages/FooPage.vue` (`<script setup lang="ts">`). Wrap data in `QueryBoundary` so loading, error,
   "not available yet" and empty states are handled; use `GateButton` for actions.
3. Register the route in `src/router/index.ts` with `meta: { title, operation, nav }`; the navigation entry and the
   role check follow from `operation`.
4. Add the endpoint handler to `src/mocks/handlers.ts` (the contract test fails otherwise), add the page to
   `tests/pages-a11y.test.ts`, and add behavioural tests for what is special about it.

## Testing

`npm test` runs everything in about 15 seconds. The tests render the real app (router, pinia, query client)
and talk to the mock API through MSW in Node, so the same handlers serve `npm run dev` and the tests.

| File                      | Covers                                                                                                                                       |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `problem.test.ts`         | Problem to message mapping for every code, field errors, retry policy                                                                        |
| `useTask.test.ts`         | Polling, progress, failure, paused, tolerance of lost polls, cleanup                                                                         |
| `roles.test.ts`           | Role hierarchy from the contract, hidden navigation, disabled actions with reason, forbidden page                                            |
| `create-database.test.ts` | Four groups, immutable-field notes, validation, submit, task, list update, focus handling                                                    |
| `setup-wizard.test.ts`    | Token, create, join, validation, wrong token, task, hand-off to sign-in                                                                      |
| `servers-page.test.ts`    | Status badges (icon and text), instances, failover and add-server flows, unsupported and error states                                        |
| `shards-grid.test.ts`     | Bucket grid for a given bucket map, filters, bucket lookup                                                                                   |
| `pages-a11y.test.ts`      | Every page: renders data, logs no Vue warnings, no axe violations                                                                            |
| `flows.test.ts`           | Sign-in/out, CSRF, expired session, add server then rebalance, workbench, security, ledger, settings, theme, distribute table, drop database |
| `tokens-contrast.test.ts` | WCAG AA contrast of the token pairs in the light and dark theme                                                                              |
| `contract.test.ts`        | Generated files up to date, mock covers every operation in the spec (count read from the spec), audit actions, SPDX headers                  |
| `contract-0-2.test.ts`    | Contract 0.2.0: stats-only landing page, n/a for null, create options, distribution columns, server groups, report detail, audit filter      |

jsdom has no layout engine, so axe cannot compute colour contrast there; that is why contrast is tested from the
tokens instead.

## Known limits of this increment

The SQL editor is a plain textarea, the replication page is a table rather than a graph, and the charts are simple
SVG lines without zoom or tooltips.
