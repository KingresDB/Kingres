<!-- SPDX-License-Identifier: PostgreSQL -->
# ADR 0017: Vue for the web panel

- Status: Accepted
- Date: 2026-11-14
- Supersedes: the frontend-framework part of [ADR 0016](0016-go-backend-react-frontend-openapi-first.md). The Go backend and the OpenAPI-first contract in ADR 0016 are unchanged.
- Source in architecture.md: "Proposed architecture (recommendation)"

## Context

ADR 0016 recorded the architecture document's recommendation of React with TypeScript for the panel. That recommendation had no source behind it; the document itself says no evidence was gathered for the frontend library choices. On the day implementation of the panel started, before any panel code existed that was worth keeping, the project owner asked for Vue instead, as the framework they prefer and consider lighter.

What the panel needs from a framework is modest: a single-page application with a typed API client, routing, forms, tables, a few charts, and a mock layer for development and tests. Both frameworks meet these needs. The pieces chosen to be framework-independent stay as they were: API types generated from `kingres/api/openapi.yaml`, a network-level mock layer, Vite as the build tool and Vitest as the test runner.

## Decision

The panel is written in Vue 3 with TypeScript: single-file components with `<script setup lang="ts">` and the Composition API, Vue Router for routing, Pinia for the small amount of shared client state, and a query library for server state. The coding rules are in [coding-standards.md](../dev/coding-standards.md), section 4.

## Consequences

- The person who owns and will read the panel code works in the framework they prefer. This was the deciding reason.
- Templates are type-checked with `vue-tsc`, which is a separate tool from the TypeScript compiler and must be part of CI.
- The topology-graph candidate named in the architecture document (React Flow) does not apply; a Vue-compatible graph library is chosen when that view is built.
- The few minutes of React scaffolding that existed were discarded; nothing else was affected, because the contract and the backend do not depend on the frontend framework.
- "Lighter" should not be overstated: the difference in framework runtime size is small next to the application's own code and its charting library, and the panel is served from the node itself. No measurement was made for this decision.

## Alternatives considered

- **React with TypeScript** (ADR 0016). Technically equivalent for this application. Rejected because the project owner prefers Vue.
- **A server-rendered panel without a client framework.** Not considered in detail; the panel needs live task progress, polling views and rich dialogs.
