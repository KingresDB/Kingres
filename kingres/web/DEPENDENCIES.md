<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

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
| `axe-core`                    | 4.14.0  | Accessibility rules engine used by `vitest-axe` and, in a real browser, by `npm run test:browser`                                  | MPL-2.0 (see below) |

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

## Production install tree

`npm ci --omit=dev` installs 123 packages, more than the five runtime packages above, because npm also installs
peer dependencies: `vue-router` 5 lists `vite` (and through it `rolldown`, `lightningcss` and `postcss`) as a peer,
and `unplugin` and `@vue-macros/common` are its own dependencies (they transform route files at build time). None
of this code is in `dist/`; `dist/` holds only the code of `vue`, `vue-router`, `pinia`, `@tanstack/vue-query`,
`openapi-fetch` and the panel (checked by reading the bundle: no `vite`, `rolldown` or `lightningcss` code, no
`msw`, no `axe`). The table lists every package of that tree with its licence and the runtime package that
needs it. Rows marked optional are platform binaries of which npm installs only the one for the machine it runs on.

| Package                                 | Version      | Licence      | Needed by             | Note                                                 |
| --------------------------------------- | ------------ | ------------ | --------------------- | ---------------------------------------------------- |
| `@babel/helper-string-parser`           | 7.29.14      | MIT          | `@tanstack/vue-query` |                                                      |
| `@babel/helper-string-parser`           | 8.1.2        | MIT          | `vue-router`          |                                                      |
| `@babel/helper-validator-identifier`    | 7.29.7       | MIT          | `@tanstack/vue-query` |                                                      |
| `@babel/helper-validator-identifier`    | 8.0.6        | MIT          | `vue-router`          |                                                      |
| `@babel/parser`                         | 7.29.14      | MIT          | `@tanstack/vue-query` |                                                      |
| `@babel/parser`                         | 8.1.2        | MIT          | `vue-router`          |                                                      |
| `@babel/types`                          | 7.29.14      | MIT          | `@tanstack/vue-query` |                                                      |
| `@babel/types`                          | 8.1.2        | MIT          | `vue-router`          |                                                      |
| `@emnapi/core`                          | 2.0.0-beta.2 | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@emnapi/runtime`                       | 2.0.0-beta.2 | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@emnapi/wasi-threads`                  | 2.2.0        | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@jridgewell/gen-mapping`               | 0.3.13       | MIT          | `vue-router`          |                                                      |
| `@jridgewell/remapping`                 | 2.3.5        | MIT          | `vue-router`          |                                                      |
| `@jridgewell/resolve-uri`               | 3.1.3        | MIT          | `vue-router`          |                                                      |
| `@jridgewell/sourcemap-codec`           | 1.6.0        | MIT          | `@tanstack/vue-query` |                                                      |
| `@jridgewell/trace-mapping`             | 0.3.31       | MIT          | `vue-router`          |                                                      |
| `@napi-rs/wasm-runtime`                 | 1.2.6        | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@oxc-project/types`                    | 0.156.0      | MIT          | `vue-router`          |                                                      |
| `@rolldown/binding-android-arm-eabi`    | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-android-arm64`       | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-darwin-arm64`        | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-darwin-x64`          | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-freebsd-x64`         | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-arm-gnueabihf` | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-arm64-gnu`     | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-arm64-musl`    | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-ppc64-gnu`     | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-s390x-gnu`     | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-x64-gnu`       | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-linux-x64-musl`      | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-openharmony-arm64`   | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-wasm32-wasi`         | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-win32-arm64-msvc`    | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/binding-win32-x64-msvc`      | 1.2.19       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@rolldown/pluginutils`                 | 1.1.1        | MIT          | `vue-router`          |                                                      |
| `@tanstack/match-sorter-utils`          | 9.2.6        | MIT          | `@tanstack/vue-query` |                                                      |
| `@tanstack/query-core`                  | 5.107.1      | MIT          | `@tanstack/vue-query` |                                                      |
| `@tanstack/vue-query`                   | 5.107.1      | MIT          | `@tanstack/vue-query` |                                                      |
| `@tybys/wasm-util`                      | 0.10.4       | MIT          | `vue-router`          | optional (platform binary or helper)                 |
| `@types/estree`                         | 1.0.10       | MIT          | `vue-router`          |                                                      |
| `@types/node`                           | 26.6.6       | MIT          | `vue-router`          |                                                      |
| `@vue-macros/common`                    | 3.1.4        | MIT          | `vue-router`          |                                                      |
| `@vue/compiler-core`                    | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/compiler-dom`                     | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/compiler-sfc`                     | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/compiler-ssr`                     | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/devtools-api`                     | 6.6.4        | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/devtools-api`                     | 8.2.1        | MIT          | `pinia`               |                                                      |
| `@vue/devtools-kit`                     | 8.2.1        | MIT          | `pinia`               |                                                      |
| `@vue/devtools-shared`                  | 8.2.1        | MIT          | `pinia`               |                                                      |
| `@vue/reactivity`                       | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/runtime-core`                     | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/runtime-dom`                      | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/server-renderer`                  | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `@vue/shared`                           | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `acorn`                                 | 8.19.0       | MIT          | `vue-router`          |                                                      |
| `ast-kit`                               | 2.2.0        | MIT          | `vue-router`          |                                                      |
| `ast-kit`                               | 3.0.1        | MIT          | `vue-router`          |                                                      |
| `ast-walker-scope`                      | 0.10.0       | MIT          | `vue-router`          |                                                      |
| `birpc`                                 | 2.9.0        | MIT          | `pinia`               |                                                      |
| `chokidar`                              | 6.0.0        | MIT          | `vue-router`          |                                                      |
| `confbox`                               | 0.1.8        | MIT          | `vue-router`          |                                                      |
| `confbox`                               | 0.3.2        | MIT          | `vue-router`          |                                                      |
| `csstype`                               | 3.2.3        | MIT          | `@tanstack/vue-query` |                                                      |
| `detect-libc`                           | 2.1.2        | Apache-2.0   | `vue-router`          |                                                      |
| `entities`                              | 7.0.1        | BSD-2-Clause | `@tanstack/vue-query` |                                                      |
| `estree-walker`                         | 2.0.2        | MIT          | `@tanstack/vue-query` |                                                      |
| `estree-walker`                         | 3.0.3        | MIT          | `vue-router`          |                                                      |
| `exsolve`                               | 1.2.0        | MIT          | `vue-router`          |                                                      |
| `fdir`                                  | 6.5.0        | MIT          | `vue-router`          |                                                      |
| `fsevents`                              | 2.3.3        | MIT          | `vue-router`          | optional (platform binary or helper); install script |
| `hookable`                              | 5.5.3        | MIT          | `pinia`               |                                                      |
| `lightningcss`                          | 1.33.0       | MPL-2.0      | `vue-router`          |                                                      |
| `lightningcss-android-arm64`            | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-darwin-arm64`             | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-darwin-x64`               | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-freebsd-x64`              | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-linux-arm-gnueabihf`      | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-linux-arm64-gnu`          | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-linux-arm64-musl`         | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-linux-x64-gnu`            | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-linux-x64-musl`           | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-win32-arm64-msvc`         | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `lightningcss-win32-x64-msvc`           | 1.33.0       | MPL-2.0      | `vue-router`          | optional (platform binary or helper)                 |
| `local-pkg`                             | 1.2.1        | MIT          | `vue-router`          |                                                      |
| `local-pkg`                             | 2.0.0        | MIT          | `vue-router`          |                                                      |
| `magic-string`                          | 0.30.21      | MIT          | `@tanstack/vue-query` |                                                      |
| `magic-string`                          | 1.4.5        | MIT          | `vue-router`          |                                                      |
| `magic-string-ast`                      | 1.0.3        | MIT          | `vue-router`          |                                                      |
| `mlly`                                  | 1.8.2        | MIT          | `vue-router`          |                                                      |
| `muggle-string`                         | 0.4.1        | MIT          | `vue-router`          |                                                      |
| `nanoid`                                | 3.3.20       | MIT          | `@tanstack/vue-query` |                                                      |
| `nostics`                               | 1.3.0        | MIT          | `pinia`               |                                                      |
| `openapi-fetch`                         | 0.17.0       | MIT          | `openapi-fetch`       |                                                      |
| `openapi-typescript-helpers`            | 0.1.0        | MIT          | `openapi-fetch`       |                                                      |
| `pathe`                                 | 2.0.3        | MIT          | `vue-router`          |                                                      |
| `perfect-debounce`                      | 2.1.0        | MIT          | `pinia`               |                                                      |
| `picocolors`                            | 1.1.1        | ISC          | `@tanstack/vue-query` |                                                      |
| `picomatch`                             | 4.0.7        | MIT          | `vue-router`          |                                                      |
| `pinia`                                 | 4.1.0        | MIT          | `pinia`               |                                                      |
| `pkg-types`                             | 1.3.1        | MIT          | `vue-router`          |                                                      |
| `pkg-types`                             | 2.4.0        | MIT          | `vue-router`          |                                                      |
| `postcss`                               | 8.5.33       | MIT          | `@tanstack/vue-query` |                                                      |
| `quansync`                              | 0.2.11       | MIT          | `vue-router`          |                                                      |
| `quansync`                              | 1.0.0        | MIT          | `vue-router`          |                                                      |
| `readdirp`                              | 5.1.1        | MIT          | `vue-router`          |                                                      |
| `remove-accents`                        | 0.5.0        | MIT          | `@tanstack/vue-query` |                                                      |
| `rolldown`                              | 1.2.19       | MIT          | `vue-router`          |                                                      |
| `scule`                                 | 1.3.0        | MIT          | `vue-router`          |                                                      |
| `source-map-js`                         | 1.2.2        | BSD-3-Clause | `@tanstack/vue-query` |                                                      |
| `tinyglobby`                            | 0.2.17       | MIT          | `vue-router`          |                                                      |
| `tslib`                                 | 2.9.1        | 0BSD         | `vue-router`          | optional (platform binary or helper)                 |
| `typescript`                            | 5.9.3        | Apache-2.0   | `@tanstack/vue-query` |                                                      |
| `ufo`                                   | 1.6.4        | MIT          | `vue-router`          |                                                      |
| `undici-types`                          | 8.9.0        | MIT          | `vue-router`          |                                                      |
| `unplugin`                              | 3.7.0        | MIT          | `vue-router`          |                                                      |
| `unplugin-utils`                        | 0.3.2        | MIT          | `vue-router`          |                                                      |
| `vite`                                  | 8.3.8        | MIT          | `vue-router`          |                                                      |
| `vue`                                   | 3.5.45       | MIT          | `@tanstack/vue-query` |                                                      |
| `vue-demi`                              | 0.14.10      | MIT          | `@tanstack/vue-query` | install script                                       |
| `vue-router`                            | 5.5.1        | MIT          | `vue-router`          |                                                      |
| `webpack-virtual-modules`               | 0.6.2        | MIT          | `vue-router`          |                                                      |
| `yaml`                                  | 2.9.1        | ISC          | `vue-router`          |                                                      |

### Install scripts

Two packages of that tree have an install script. Nothing else in the lockfile, development packages included, has one.

- `vue-demi` 0.14.10 (needed by `@tanstack/vue-query`): `postinstall` runs `node -e "try{require('./scripts/postinstall.js')}catch(e){}"`.
  The script (read from the installed package) loads `vue`, looks at its version, and calls `switchVersion`, which
  copies the matching entry files (Vue 2, 2.7 or 3) over `vue-demi/lib/index.*` inside the `vue-demi` directory itself; a
  failure is swallowed. Acceptable because it reads no network and no environment, writes only inside its own package
  directory, and the package is MIT and pinned by the lockfile. npm 11.19 reports it as not covered by
  `allowScripts` (the project sets none); in the installed tree `lib/index.mjs` is identical to `lib/v3/index.mjs`, the
  variant the script selects for Vue 3.
- `fsevents` 2.3.3 (an optional dependency of `vite`): builds a macOS file-watching binary and is skipped on every other
  system. MIT.

## Licence notes

Outside MIT, BSD, Apache-2.0 and ISC:

- **MPL-2.0 (weak, file-level copyleft), production tree:** `lightningcss` 1.33.0 and its 11 platform binaries
  (`lightningcss-<platform>`). Vite uses it to minify CSS at build time. They are installed because `vite` is a peer of
  `vue-router`, but none of their code is in `dist/` (the minified CSS contains a few `--lightningcss-*` helper custom
  properties that the minifier writes; that is its output, not its code). We do not modify or redistribute them.
  The project owner accepted them as build tools.
- **MPL-2.0, development only:** `axe-core` 4.14.0 (accessibility tests). Not installed with `--omit=dev`, not in `dist/`.
- **0BSD, production tree (optional):** `tslib` 2.9.1, needed by `@emnapi/*` and `@tybys/wasm-util`, which are the
  WebAssembly fallback of the `rolldown` build tool. 0BSD is a permissive licence with no conditions. Not in `dist/`.
- **BlueOak-1.0.0, development only (9 packages):** `glob` 13.0.7, `minimatch` 11.0.8 and the copies 10.2.6 under
  `@typescript-eslint/typescript-estree`, `editorconfig` and `glob`, `lru-cache` 11.5.3, `minipass` 7.1.3,
  `path-scurry` 2.0.2 and `@isaacs/nonrecursive-glob-match` 1.1.0. The Blue Oak Model License is a plain permissive
  licence (use, copy, modify and distribute with the notice). They are used by ESLint, `typescript-eslint` and
  `js-beautify` on the developer's machine and are not in `dist/`.
- **Python-2.0, development only:** `argparse` 2.0.1 (used by `js-yaml` in the lint tools). A permissive licence
  (the PSF licence); it only ever runs on the developer's machine.
- **(MIT OR CC0-1.0), development only:** `type-fest` 5.10.0 and 4.41.0 (types only).

## Known vulnerabilities (`npm audit`)

- `npm audit --omit=dev` (what is installed for a build of the shipped panel): **0 vulnerabilities**.
- `npm audit` (with the development tools): 11 moderate findings, all development-only and all from two advisories
  that are denial of service through crafted glob patterns: `minimatch` 7.0.0 to 11.0.0
  (GHSA-g3hv-75wg-m249; the copies 10.2.6 under `@typescript-eslint/typescript-estree` and `editorconfig`) and `glob`
  9.0.0 to 14.0.1 (GHSA-3g3m-2fjr-6pw4, used by `js-beautify`, which `@vue/test-utils` uses to format HTML in
  failure messages). The affected tools only ever read patterns from this repository (the ESLint configuration and the
  test files), never from the network or from a user, and none of them is part of `dist/`. The only fix npm offers is a
  downgrade of `typescript-eslint` to 8.36.0 (a breaking change that loses rules), and an `overrides` entry that forces
  `minimatch` 11 was tried and not accepted by npm for `@typescript-eslint/typescript-estree` (its own range is
  `^10`). Accepted as development-only until `typescript-eslint` and `@vue/test-utils` release versions that depend on
  the fixed packages; `npm audit` is re-run whenever the lockfile changes.

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
