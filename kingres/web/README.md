<!-- SPDX-License-Identifier: PostgreSQL -->

# KingresDB web panel

The management console of KingresDB: a single-page application (Vue 3, TypeScript, Vite) that is a client
of the REST API in `kingres/api/openapi.yaml` (revision 0.5.0, 88 operations; the generated files follow the committed 0.5.0, see "Contract revisions" below) and nothing else. The Go server (`kingresd`) will embed the
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

| Parameter                                          | Effect                                                                                                                                                                                                                                              |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `?mock=uninitialized`                              | Starts as a fresh server and shows the first-run wizard. The setup token is `demo-setup-token`. A cluster named `failme` makes setup fail (to see the retry).                                                                                       |
| `?unsupported=createInclusionProof,listAlerts`     | Those operations answer `unsupported_operation`, to see the "not available on this server version yet" state.                                                                                                                                       |
| `?mock=hostile`                                    | Adds names and messages as a low-privileged database user could make them (markup, bidirectional and zero-width characters, look-alike letters, 5000-character names, a database called `..`).                                                      |
| `?partial=10.0.0.12:timeout,10.0.0.13:unreachable` | Logs, alerts and metrics answer as partial answers (contract 0.5.0): `partial: true` and `missingServers` with the given ids and reasons (`timeout`, `unreachable`, `refused`, `invalidAnswer`). `?mock=hostile` does this with hostile server ids. |
| `?cursors=reject`                                  | Every request that carries a cursor answers 400, as a 0.5.0 server does for a cursor of 0.4.0.                                                                                                                                                      |
| `?latency=0`                                       | No artificial delay (the default is 150 ms).                                                                                                                                                                                                        |

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

| Script                        | What it does                                                                                                                                                                                                    |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `npm run dev` / `dev:backend` | Dev server with the mock API / with the proxy to `127.0.0.1:8091`                                                                                                                                               |
| `npm run build`               | Type-check, then build to `dist/` (the directory the Go server embeds)                                                                                                                                          |
| `npm run typecheck`           | `vue-tsc --noEmit` (scripts and templates, `strict`, `noUncheckedIndexedAccess`)                                                                                                                                |
| `npm run lint`                | ESLint (flat config, Vue and Vue accessibility plugins) and `prettier --check`                                                                                                                                  |
| `npm run lint:fix` / `format` | Fix lint problems / format with Prettier                                                                                                                                                                        |
| `npm test`                    | Vitest and Testing Library, against the mock API (jsdom, no browser needed)                                                                                                                                     |
| `npm run test:browser`        | The tests that need a real Chrome or Edge: layout at 1366, 683 and 320 px, keyboard, axe, and the production build under the content security policy (needs `CHROME_PATH` if Chrome is not in a standard place) |
| `npm run api:generate`        | Regenerate the API types from the contract                                                                                                                                                                      |
| `npm run api:check`           | Fail if the generated files are out of date (also run by `npm test`)                                                                                                                                            |
| `npm run brand:sync`          | Copy the favicons from `kingres/brand/` into `public/` and regenerate the logo path data                                                                                                                        |
| `npm run brand:check`         | Fail if the committed brand files differ from `kingres/brand/` (also run by `npm test`)                                                                                                                         |

## End-to-end test against the real server

`npm run e2e` is one command. It needs Windows with WSL2 (distribution `Ubuntu-24.04`, user `kingres`), Go, Chrome or Edge, and
an installed KingresDB with the current extensions inside WSL (`~/kw-cp/install` when it has `kingres_ledger` 0.2, else
`~/kingres-work/install`; `E2E_PG_INSTALL` names another). It:

1. builds the production panel (`vite build`), then `kingresd` for Linux from `kingres/control-plane` (or uses `KINGRESD_BIN`) with
   the panel embedded: a Go build overlay puts `dist/` in place of the placeholder in `internal/webui/dist` for this build only,
   so nothing in the control plane is changed, and the panel the browser sees is served by the server with its own headers,
2. copies the installation to `~/kw-web/install`, starts `kingresd` there on a fresh data directory (API port 58201,
   PostgreSQL port 58200; override with `E2E_API_PORT`, `E2E_PG_PORT`),
3. also builds the standalone verifier `kingres-verify` (`kingres/verifier`) for this machine,
4. drives a headless browser through the real flow (`scripts/e2e/flow.mjs`): wizard with the real setup token (wrong
   token and wrong data directory first), sign-in, reload, roles (duplicate, disable, a stats-only account), a sharded
   database with the ledger, tables made with psql, distribute and enable-ledger from the panel, workbench query and
   SQL error, grant, network rule, every page, calm notices for the operations the server does not implement yet (failover,
   rebalance), the cluster settings, the audit log, drop database, server groups (create, assign, refused delete, unassign,
   delete), the certificate and fingerprint of a server, the add-server dialog with a fingerprint, a ledger proof (pending
   until a digest, then shown), the proof and digest files compared byte for byte with the server's answer and accepted by
   `kingres-verify`, a ledger verification and its record, local settings (log level, memory quota and the persistent
   restart-required state), a restart of the instance, stopping the instance that hosts the system database (one clear state,
   no error storm, start from the panel), the leave-cluster refusal for the only server, the audit log of these, the headers
   (security headers, caching, cookie attributes, no Content-Security-Policy violation on any page), a server restart that
   signs everyone out, and the stats-only and read-only accounts,
5. prints a report (notes, per-page state, every HTTP error the browser saw) and tears everything down.

The exit code is 0 only if every step passed. Screenshots are in `.e2e/shots/` (ignored by git). `E2E_KEEP_INSTALL=1`
keeps `~/kw-web/install` for faster re-runs; `E2E_KEEP_RUNNING=1` leaves the processes running. `scripts/e2e/backend.sh` is
the WSL side (`prepare`, `start`, `restart`, `token`, `psql`, `stop`). `puppeteer-core` is a development dependency.

### What the run against kingresd 0.2.0-dev found (2026-11-15, contract 0.4.0)

All 44 steps pass against the real server, with the panel served by the server itself. The mismatches of the earlier run
(ledger status 404, grants 404, `plain` combined with `ledger`, `isManagerLeader`, the database count) are fixed in the server
("Corrections after the panel's end-to-end run" in its README). What differs now, none of it needing a workaround in the panel:

- The server also sends `Cross-Origin-Resource-Policy: same-origin` on every response (see "What the server must send").
  Every other header, the caching of every class of response (index, hashed assets, other static files, the API) and the
  attributes of the session cookie (`HttpOnly; Secure; SameSite=Strict; Path=/`, cleared on sign-out) are as listed, and the
  browser reported no Content-Security-Policy violation on any page, dialog or download.
- With TLS on the server sends `Strict-Transport-Security: max-age=63072000; includeSubDomains`; this README only recommended
  `max-age=31536000`. The server's value is the stronger one.
- During an instance restart or stop there is a moment (about a second) in which a request answers 503 `service_unavailable`
  ("terminating connection due to administrator command") before the server answers 503 `system_database_stopped`. The panel
  treats both calmly.
- `kingresd` must be started with `-internal-listen` (the REST port plus one) for leave-cluster, add-server and remove-server.
  Without it they answer 422 `unsupported_operation` ("This server cannot add or remove servers (no internal API or instance
  management)"), which looks like "not implemented yet" in the panel. The end-to-end script passes the flag.

## What the server must send

The panel is static files plus calls to `/api/v1/` on the **same origin**. It sets no security header itself (a
`<meta>` tag cannot set `frame-ancestors`, and caching is the server's business), so `kingresd` has to. This section is
written to be implemented without reading anything else. The values are checked: `tests/server-headers.test.ts` fails
when a value here differs from `browser-tests/server-headers.ts`, and `npm run test:browser` (`csp.test.ts`) builds
`dist/`, serves it with exactly these headers, and visits every page, the sign-in page, the first-run wizard and the
dialogs with hostile data: there is **no** policy violation and no console message about one.

### Headers on every response (the files of the panel and `/api/v1/`)

| Header                       | Value                                                                                                                                                                                          |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Content-Security-Policy`    | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` |
| `X-Frame-Options`            | `DENY` (old browsers; `frame-ancestors 'none'` above is the real protection)                                                                                                                   |
| `X-Content-Type-Options`     | `nosniff`                                                                                                                                                                                      |
| `Referrer-Policy`            | `no-referrer` (task ids, database and server names are in the path, and the sign-in redirect is in the query)                                                                                  |
| `Cross-Origin-Opener-Policy` | `same-origin`                                                                                                                                                                                  |
| `Permissions-Policy`         | `camera=(), microphone=(), geolocation=()`                                                                                                                                                     |

The exact lines, to copy:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

kingresd also sends `Cross-Origin-Resource-Policy: same-origin` on every response (the panel does not need it; nothing else may embed the responses) and, with TLS on, `Strict-Transport-Security`.

What the policy relies on, so that nobody weakens it by accident: the build has no inline script and no inline style
element, no `eval`, no `data:` URL, no web font, no external origin and no service worker. `'unsafe-inline'` and
`'unsafe-eval'` are **not** needed and must not be added. Vue sets styles through the CSSOM, which `style-src` does not
block; a `style="..."` attribute written into a statically hoisted template would be blocked (`style-src-attr`), so
the browser test fails if one appears. If the server also serves the panel under another path prefix or over another
origin, `connect-src` and `default-src` must name it (the panel only ever calls its own origin).

### Caching

| Response                                                                                                                   | `Cache-Control`                       |
| -------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- |
| `index.html` and every path that falls back to it (`/login`, `/servers`, ...)                                              | `no-cache`                            |
| Files under `/assets/` (their names contain a content hash)                                                                | `public, max-age=31536000, immutable` |
| Every response of `/api/v1/`, errors included (the data is sensitive and depends on the role)                              | `no-store`                            |
| `/favicon.svg`, `/favicon-32.png`, `/apple-touch-icon.png`, `/icon-512.png`, `/manifest.webmanifest` (no hash in the name) | `no-cache`                            |

### The session cookie and the CSRF token

- `kingres_session=<value>; HttpOnly; Secure; SameSite=Strict; Path=/`, as the contract states. The panel works over
  HTTPS and at `http://localhost` (browsers keep a `Secure` cookie there); over plain HTTP on another host the browser
  drops the cookie and the panel says so after sign-in.
- The CSRF token comes in the JSON body of `login` and `GET /auth/session` and is held in memory only. The server must
  require `X-CSRF-Token` on **every** non-GET request, and must not accept `login` from another site (`SameSite=Strict`
  on the cookie covers it; a cross-site login would otherwise sign a victim into the attacker's account).
- A restart of the server or an expired session must answer `401` with `code: "session_expired"` or `"unauthenticated"`
  on **every** protected path (also with an HTML body from a proxy: the panel treats any `401` as the end of the session).

### Routing

- Serve `index.html` for every path that is not a file and does **not** start with `/api/`, so that a reload on
  `/databases/shop` works. An unknown path under `/api/` must answer a JSON problem (`404 not_found`), never HTML.
- Path parameters arrive percent-encoded once (`/api/v1/databases/a%2Fb%3Fc%23d%25e` is the database `a/b?c#d%e`). The
  router of the server must decode a segment after splitting the path, and must not treat `%2F` as a separator. A
  name that is `.` or `..` (or empty) cannot be sent by a browser at all (the URL parser removes it, also written
  `%2E`): the panel refuses to send such a request and shows the object as "cannot be opened by address".
- Do not serve `mockServiceWorker.js` or any development file; the production build contains none (the browser test
  checks the bundle for the mock).
- Recommended in addition (not tested here): `Strict-Transport-Security: max-age=31536000` when the panel is served over
  HTTPS.

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
  bootstrap.ts  builds pinia + query client + router, and the one end-of-session path (used by main.ts and the tests)
tests/          Vitest; helpers.ts renders the real app against the mock API
browser-tests/  Real-browser tests (puppeteer-core, own vitest config): layout, keyboard, axe, content security policy
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
- **Names and messages from the database** (roles, databases, schemas, tables, columns, server groups, and the text of
  logs, alerts, audit entries, tasks and errors) are chosen by whoever may create them, so they are shown only through
  `SafeName` (or `safeText()` where markup cannot go, such as an `<option>` or an `aria-label`). It makes bidirectional
  controls, zero-width characters and control characters visible as `U+202E`-style marks with a description, isolates the
  text direction, flags names that mix scripts or use look-alike letters, and folds very long values with "Show all N
  characters". Confirmations show the escaped name and still require the real one (with a "Copy the name" button).
  Nothing is stripped or normalised before it is sent. `tests/hostile.test.ts` goes through every page with hostile data.
- **One end of session.** Every way a session can end (sign-out, expiry seen by any query, mutation or task poll, a
  different user in the session, another tab signing in) goes through `session.adopt`, which calls
  `resetServerState()` (`src/api/server-state.ts`): the query cache, the in-flight requests, the task pollers and the task
  notices are cleared or cancelled. A new store that holds API data registers its own reset there.
- **Path parameters** are encoded in one place (`pathSerializer` in `src/api/client.ts`). A value that no URL can carry
  (`.`, `..`, empty) is refused with an error before any request is made.
- **Forms.** A form whose action changes the whole cluster, or cannot be undone, has no implicit submission
  (`v-block-enter-submit` from `src/lib/forms.ts`) and a button that opens a confirmation; dialog forms with their own
  visible submit button (create role, create server group, sign-in) keep Enter. Fields that were not edited are not sent.
- **Dates** from the API are UTC and are shown as UTC (`formatDateTime` adds "UTC"); a date field says "(UTC)" and
  is read as UTC; an untouched date is not sent back.
- **Partial cluster-wide answers (0.5.0).** `listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster; a
  server that did not answer is named in `missingServers` with a reason. The views show `PartialNotice` ("This list is
  incomplete", every server and the reason in plain words, server ids through `SafeName`), announce it once through the
  live region (`useCompletenessAnnouncement`: when the set of missing servers changes, not on every poll), remove it when a
  later answer is complete, and never show a partial empty list as "No alerts" or "No log lines" (the empty state says
  "from the servers that answered" and that this does not mean there are none).
- **Cursors (0.5.0 changed their format).** A cursor lives only in the memory of one query (`usePagedQuery`): never in the
  address, never in browser storage, so a reload or a new session starts at the first page. A 400 for a request that
  carries a cursor is not an error dead end: the list starts again from the first page and says so
  (`CursorResetNotice`); `fetchAllPages` restarts once, silently.
- **Alert ids** are integers below 2^53 and are never parsed, rounded or re-serialized by the panel (tested with 2^52+1);
  an id that a JSON number cannot hold exactly is not acknowledged from the panel.
- **What is kept in `sessionStorage`** (this tab only, gone with the tab): the session as `login`/`getSession` returned it
  (with its CSRF token) and the id of the instance that hosts the system database. They are used for one thing: after a
  reload while the system database is stopped, `getSession` answers 503 and the contract still lets the existing session
  start the instance; the panel then keeps the session and shows the stopped state with a Start button (see below). The
  server decides in every other case, and both are removed when the session ends. `localStorage` still holds only the
  theme. The stopped state is told to the other tabs through `BroadcastChannel` (a `storage` event where there is none):
  one boolean, no session, no token.
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

| File                                                                                                                           | Covers                                                                                                                                                                              |
| ------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `problem.test.ts`                                                                                                              | Problem to message mapping for every code, field errors, retry policy                                                                                                               |
| `useTask.test.ts`                                                                                                              | Polling, progress, failure, paused, tolerance of lost polls, cleanup                                                                                                                |
| `roles.test.ts`                                                                                                                | Role hierarchy from the contract, hidden navigation, disabled actions with reason, forbidden page                                                                                   |
| `create-database.test.ts`                                                                                                      | Four groups, immutable-field notes, validation, submit, task, list update, focus handling                                                                                           |
| `setup-wizard.test.ts`                                                                                                         | Token, create, join, validation, wrong token, task, hand-off to sign-in                                                                                                             |
| `servers-page.test.ts`                                                                                                         | Status badges (icon and text), instances, failover and add-server flows, unsupported and error states                                                                               |
| `shards-grid.test.ts`                                                                                                          | Bucket grid for a given bucket map, filters, bucket lookup                                                                                                                          |
| `pages-a11y.test.ts`                                                                                                           | Every page: renders data, logs no Vue warnings, no axe violations                                                                                                                   |
| `flows.test.ts`                                                                                                                | Sign-in/out, CSRF, expired session, add server then rebalance, workbench, security, ledger, settings, theme, distribute table, drop database                                        |
| `tokens-contrast.test.ts`                                                                                                      | WCAG AA contrast of the token pairs in the light and dark theme                                                                                                                     |
| `contract.test.ts`                                                                                                             | Generated files up to date, mock covers every operation in the spec (count read from the spec), audit actions, SPDX headers                                                         |
| `session-reset.test.ts`                                                                                                        | Every way a session ends clears the client (sign-out, expiry on a query, a mutation or a task poll, another user, another tab)                                                      |
| `api-client.test.ts`                                                                                                           | Exact request URLs for hostile path parameters, answers that are not problem documents, unknown problem codes, the sign-in redirect allow-list                                      |
| `task-notices.test.ts`                                                                                                         | A hidden task is still followed and its failure reported; typed confirmations for remove server, hard failover, disable role                                                        |
| `network-access.test.ts`, `network-rules.test.ts`                                                                              | Network rules: no Enter submission, confirmation with the changes, lock-out warnings, lists that round-trip                                                                         |
| `grants.test.ts`, `role-dialog.test.ts`                                                                                        | A failed grant is not shown as granted, template confirmation, an unchanged role is not sent (also under another time zone)                                                         |
| `workbench.test.ts`                                                                                                            | NULL, the text NULL and the empty string, stale results, timeout, stop waiting, large results, duplicate columns                                                                    |
| `plumbing.test.ts`                                                                                                             | Unreadable ledger status shown, cursor paging with a bound and a "more results" notice                                                                                              |
| `hostile.test.ts`                                                                                                              | Every list and detail page and dialog with hostile names and messages                                                                                                               |
| `forms.test.ts`, `dialogs.test.ts`, `task-poller.test.ts`                                                                      | Enter in forms, what is sent on save, inert page behind a dialog, live regions, no polling in a hidden tab                                                                          |
| `dependencies.test.ts`                                                                                                         | `DEPENDENCIES.md` names every package of the production tree and every exceptional development package                                                                              |
| `server-headers.test.ts`                                                                                                       | The values in "What the server must send" are the ones the browser test uses                                                                                                        |
| `contract-0-2.test.ts`                                                                                                         | Contract 0.2.0: stats-only landing page, n/a for null, create options, distribution columns, server groups, report detail, audit filter                                             |
| `contract-0-4.test.ts`                                                                                                         | Contract 0.4.0: every problem code has words and a mock status, new required properties of Task and Server                                                                          |
| `servers-0-4.test.ts`                                                                                                          | Certificate and fingerprint display, expected fingerprint (add, join), server groups (create, assign, refused delete), remove server                                                |
| `ledger-0-4.test.ts`, `raw-json.test.ts`                                                                                       | Proof and digest files byte for byte, pending proofs, refusals, verification list and detail                                                                                        |
| `this-server-instances.test.ts`, `system-database-stopped.test.ts`, `local-settings.test.ts`, `leave-cluster.test.ts`          | Instance start, stop and restart with their refusals, the stopped-system-database state, local settings, leave cluster                                                              |
| `certificates-0-5.test.ts`                                                                                                     | Renew now (ordinary confirmation, task), Revoke certificates (what happens, typed server id, nothing pre-ticked), revoked marks, blocked actions with reasons, the two new problems |
| `partial-answers.test.ts`, `alerts-0-5.test.ts`                                                                                | Partial notices on Logs, Alerts and the dashboard, announced once; alert kinds and links; alert ids above 2^52; cursors never kept and a 400 starts again                           |
| `ledger-0-5.test.ts`, `system-state-sharing.test.ts`, `hostile-0-5.test.ts`                                                    | Digest paging, digest against proof (identifiers only), the stopped state across tabs, the hostile seed over the 0.5.0 places                                                       |
| `review-0-5.test.ts`                                                                                                           | One test per finding of the second review (W2-01 to W2-16 and the residuals), named by finding                                                                                      |
| `task-detail.test.ts`, `kind-labels.test.ts`, `role-guards.test.ts`, `unsupported-operations.test.ts`, `workbench-0-4.test.ts` | Task detail text, kind labels, log access needs `kingres_ops`, unsupported operations stay calm and start working when supported                                                    |

jsdom has no layout engine, so axe cannot compute colour contrast there; that is why contrast is tested from the
tokens instead.

## Contract revisions and the checks against them

The generated files (`src/api/schema.d.ts`, `src/api/operations.generated.ts`) follow the **committed** contract, 0.5.0.
The working tree of `kingres/api/openapi.yaml` can hold a draft of the next revision (0.6.0 was being prepared while this
increment was written); the panel does not chase a draft. `npm run api:check` and the contract tests compare with the working
tree, so they fail as soon as the draft differs from the generated files; that failure says nothing about the panel. To check
against the committed revision, point `KINGRES_API_SPEC` at a copy of it:

```
git show HEAD:kingres/api/openapi.yaml > /tmp/openapi-0.5.0.yaml   # outside the repository
KINGRES_API_SPEC=/tmp/openapi-0.5.0.yaml npm run api:check
KINGRES_API_SPEC=/tmp/openapi-0.5.0.yaml npm test
```

`scripts/gen-api.mjs` and the contract tests read the same variable, so `npm run api:generate` with it set regenerates from that
copy. Adapting to the next revision is a separate step.

## What the panel does for contract 0.5.0

- **Certificates of a server** (server page, "Certificate and first contact"): _Renew now_ (`renewServerCertificates`, ordinary
  confirmation, follows the task `certificate.renew`) and _Revoke certificates_ (`revokeServerCertificates`, `kingres_admin`).
  The revoke dialog says what the contract and the control plane README say: every other server and every database instance
  refuses the server in both directions, open connections run until they end, the server never gets a new certificate, the only
  way back is to remove it and join it again, which works only for a server that holds no bucket copy (so for a server with data
  it is a one-way door), the leader's own certificates cannot be revoked. It needs the server id typed and has nothing
  pre-ticked. A revoked server is marked "Certificates revoked" with an icon in the list and on its page; Renew and Revoke are
  disabled with the reason written next to them (revoked, or the manager leader); `certificate_revoked`,
  `certificate_authority_unavailable` ("the manager leader does not hold the CA key; nothing was changed") and
  `server_hosts_map_group` have plain words.
- **Partial answers, alert kinds, cursors, alert ids:** see "Conventions". An alert that names a server links to it (when the id
  can be put in a URL); the new kinds have labels.
- **Ledger:** the digest list pages ("Load more digests"). When a proof is on screen, every listed digest says whether it has the
  proof's chain (`chain_id` of the document of the chain with the proof's `shard_group_id`) and is not older than the proof's
  block; saving a digest repeats that. It compares identifiers only and says so; the check is `kingres-verify`.
- **System database stopped:** the state is shared across tabs; after a reload the stopped state is shown with a Start button
  that names the instance it starts (#N, learned from the instance list while the server answered, never guessed); with no
  saved session (a new tab, another browser) the sign-in page explains what to do (see the limits).

## Known limits of this increment

The SQL editor is a plain textarea, the replication page is a table rather than a graph, and the charts are simple
SVG lines without zoom or tooltips.

What the contract (revision 0.5.0) does not let the panel do, and what the panel does instead:

- **Cancel a workbench statement.** There is no way to cancel it on the server. "Stop waiting" aborts the request in the
  browser and says that the statement may still run until it finishes or reaches its timeout (the person chooses the
  timeout; the default of the server is 30 s). A cancel operation (for example a `queryId` in the answer and
  `DELETE /query/{queryId}`) is needed.
- **Read-only workbench.** The API has no read-only mode, so the workbench says "Mode: read-write" and that INSERT,
  UPDATE, DELETE and DDL are executed. A `readOnly` flag on `QueryRequest` (a read-only transaction) is needed.
- **Truncation marker for names.** A result cell, a log line or a name cannot say that the server cut it; the panel cuts
  what it draws (with "Show all N characters") but cannot tell a cut value from a complete one.
- **The address of the signed-in administrator.** The session does not carry it. The network-rules confirmation reads the
  last address the audit log has for the account, and says that it cannot know more (a proxy, another network path).
  A `clientAddress` in `Session`, and a dry-run of `replaceNetworkAccess` that answers "would lock the caller out", are
  needed.
- **Names that no URL can carry** (`.` and `..`). They are listed without a link and the router refuses them, but they cannot
  be opened. The server should refuse to create such a database or table through the API, or define an escape for them.
- **Paging.** The contract does not paginate `GET /databases`, `/servers`, `/server-groups` and
  `/this-server/instances` ("Not paginated"); the panel shows what they return and cannot tell if a very large cluster
  makes them long. Every list that has cursors can be paged ("Load more"); the lists that are read completely
  (roles, grants, rebalance reports) are followed up to a bound and say when more exists. A refused cursor restarts them
  without a notice.
- **Start after a reload with no saved session.** While the system database is stopped, `getSession`, `login` and
  `listThisServerInstances` answer 503, and `startThisServerInstance` needs the CSRF token that only `login` and
  `getSession` return. A tab that had the session keeps working (the panel keeps the session of the tab in
  `sessionStorage`); a new tab or another browser cannot start the instance and the sign-in page says what to do: start the
  instance on the machine of the server (for example `pg_ctl start`), or restart the `kingresd` service, which starts it
  again. The panel of another server cannot help: it starts only the instances of its own server. The contract offers
  nothing else; wishes are in the report of this increment (for example: `getSession` and `listThisServerInstances` keep
  answering from memory while the system database is stopped, or `startThisServerInstance` for "the instance that hosts the
  system database" without an id and without a CSRF token for a bearer-less cookie session).
- **The join wizard cannot say that a pin was checked.** `SetupTaskSummary` carries no detail, so after a join the wizard says
  that the fingerprint was sent and that the server does not report whether it checked it. A `detail` on the setup task is
  expected with the next contract revision.
- **A server's own fingerprint.** No operation returns the fingerprint of the server being added or joined, so the panel tells
  the administrator where to read it (the log line "bootstrap certificate of the internal API", or the SHA-256 of the
  certificate file in the data directory). A field in `GET /this-server` would let the panel show it.
- **The id of the system-database instance** is known only if this tab read the instance list while the server answered (the
  shell reads it once signed in, and every 3 s on the This server page). Otherwise the stopped state offers no button.
- **The end-to-end run** (`npm run e2e`) was not repeated for this increment: it builds `kingresd` from the working tree, which
  was being edited. The real server's answers for the new operations, for `partial` and for the cursor refusal are known from
  the contract text only.
- **Opaque to the panel:** the manager leader's CA key. `certificate_authority_unavailable` is explained, not predicted: the panel
  cannot tell beforehand whether a revoke will be refused.
