<!-- SPDX-License-Identifier: PostgreSQL -->

# Web panel dependencies

Policy (coding-standards.md, section 4): small, maintained, permissively licensed packages; versions are
pinned by `package-lock.json`; every choice is recorded here. No GPL or AGPL package is used, directly or
transitively (checked over the whole installed tree: MIT, ISC, BSD, Apache-2.0, BlueOak-1.0.0, 0BSD-like
and CC0 only, plus the three MPL-2.0 packages listed under "Licence notes").

No UI kit, no charting library, no form library and no headless-UI library is used. The panel's own
components (dialog, tabs, charts, bucket grid) are small and in `src/components/`; this keeps the
initial download at about 64 KiB gzip.

## Runtime (shipped in `dist/`)

| Package               | Version | Purpose                                                                                                                                                                                                                      | Licence |
| --------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- |
| `vue`                 | 3.5.45  | UI framework (ADR 0017)                                                                                                                                                                                                      | MIT     |
| `vue-router`          | 5.5.1   | Routing; every page is a lazy-loaded chunk                                                                                                                                                                                   | MIT     |
| `pinia`               | 4.1.0   | Client-only shared state: session and theme (ADR 0017)                                                                                                                                                                       | MIT     |
| `@tanstack/vue-query` | 5.107.1 | Server state: caching, polling, invalidation. Chosen over SWRV and hand-written caches because it has per-query polling intervals that pause in background tabs, retry control per error code, and cache invalidation by key | MIT     |
| `openapi-fetch`       | 0.17.0  | Tiny (about 2 KiB) typed `fetch` wrapper; request and response types come from the generated `schema.d.ts`, so a path, parameter or body that is not in the contract does not compile                                        | MIT     |

## Build and type generation (development only)

| Package              | Version | Purpose                                                                                                                                       | Licence    |
| -------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| `vite`               | 8.3.8   | Dev server (with `/api` proxy) and production build                                                                                           | MIT        |
| `@vitejs/plugin-vue` | 6.0.11  | Single-file components                                                                                                                        | MIT        |
| `typescript`         | 5.9.3   | Type checking. Pinned to 5.9: `openapi-typescript` declares a peer of `^5.x` and TypeScript 6/7 are rejected by it and by `typescript-eslint` | Apache-2.0 |
| `vue-tsc`            | 3.3.14  | `typecheck`: checks templates as well as scripts                                                                                              | MIT        |
| `openapi-typescript` | 7.13.0  | Generates `src/api/schema.d.ts` from `kingres/api/openapi.yaml`                                                                               | MIT        |
| `yaml`               | 2.9.1   | Reads `x-required-role` from the contract for `src/api/operations.generated.ts`                                                               | ISC        |
| `puppeteer-core`     | 25.17.0 | Drives a headless Chrome in `npm run e2e` only; it downloads no browser                                                                       | Apache-2.0 |

## Mock API and tests (development only)

| Package                       | Version | Purpose                                                                                                                            | Licence             |
| ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `msw`                         | 3.0.5   | Network-layer mock API: Service Worker in the browser (`npm run dev`), Node interceptor in tests. Not part of the production build | MIT                 |
| `vitest`                      | 5.1.0   | Test runner                                                                                                                        | MIT                 |
| `jsdom`                       | 31.0.1  | DOM for tests                                                                                                                      | MIT                 |
| `@testing-library/vue`        | 8.1.0   | Component tests through roles and labels                                                                                           | MIT                 |
| `@testing-library/user-event` | 14.6.10 | Realistic keyboard and pointer input                                                                                               | MIT                 |
| `@testing-library/jest-dom`   | 7.0.1   | DOM matchers (`toBeInTheDocument`, `toHaveAccessibleDescription`)                                                                  | MIT                 |
| `@vue/test-utils`             | 2.5.1   | Peer of `@testing-library/vue`                                                                                                     | MIT                 |
| `vitest-axe`                  | 0.1.0   | `toHaveNoViolations` matcher                                                                                                       | MIT                 |
| `axe-core`                    | 4.14.0  | Accessibility rules engine used by `vitest-axe`                                                                                    | MPL-2.0 (see below) |

## Lint and format (development only)

| Package                             | Version | Purpose                                          | Licence |
| ----------------------------------- | ------- | ------------------------------------------------ | ------- |
| `eslint`                            | 10.15.0 | Linting (flat config)                            | MIT     |
| `@eslint/js`                        | 10.0.1  | Base rules                                       | MIT     |
| `typescript-eslint`                 | 8.72.2  | TypeScript rules (`no-explicit-any` is an error) | MIT     |
| `eslint-plugin-vue`                 | 10.12.1 | Vue rules (script setup, typed props and emits)  | MIT     |
| `eslint-plugin-vuejs-accessibility` | 2.6.3   | Accessibility rules for templates                | MIT     |
| `vue-eslint-parser`                 | 10.4.2  | Parser for `.vue` files                          | MIT     |
| `globals`                           | 17.14.0 | Global names per environment                     | MIT     |
| `prettier`                          | 3.10.1  | Formatting                                       | MIT     |
| `eslint-config-prettier`            | 10.1.8  | Turns off rules that conflict with Prettier      | MIT     |
| `@types/node`                       | 26.6.6  | Node types for `vite.config.ts` and tests        | MIT     |

## Licence notes

- **MPL-2.0 (weak, file-level copyleft):** `axe-core` (accessibility tests), `lightningcss` (pulled in by
  Vite for CSS handling) and its platform binary. The project owner accepted them as development-only tools,
  because no code of them ships in `dist/` (checked: no `axe` string in the build output; the minified CSS
  contains a few `--lightningcss-*` helper custom properties that the CSS minifier generates, which is its output, not
  its code). We do
  not modify or redistribute them.
- `type-fest` is `(MIT OR CC0-1.0)`; transitive, development only.

## Brand assets

The logo files in `kingres/brand/` are supplied by the project owner and copied by `npm run brand:sync`; they are not an
npm dependency. No package was added for the logo work.

## Considered and not used

- **Component kits** (PrimeVue, Vuetify, Element Plus, Naive UI): hundreds of KiB, their own visual
  language; the panel needs about ten components.
- **ECharts / uPlot** (named in the architecture document as candidates): ECharts is far larger than the whole
  panel; uPlot is small but a 100-line SVG line chart covers the four dashboard charts. Revisit when the Shards
  heat map or time-range zooming needs canvas performance.
- **vee-validate / zod:** the constraints are few and come straight from the contract; they live in
  `src/lib/validators.ts` and the server's field errors are shown as well.
- **Headless UI / Radix Vue:** the only complex primitives are the dialog and tabs, both implemented to the
  WAI-ARIA patterns and covered by tests.
- **Playwright / Cypress:** `npm run e2e` uses `puppeteer-core` (no browser download, small) with the Chrome or
  Edge already on the machine; the larger frameworks add nothing this flow needs.
