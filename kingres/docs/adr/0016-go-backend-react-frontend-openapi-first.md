<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0016: Go backend and React/TypeScript frontend, OpenAPI first

- Status: Accepted; the frontend-framework part is superseded by [ADR 0017](0017-vue-for-the-web-panel.md)
- Date: 2026-11-14
- Source in architecture.md: "Proposed architecture (recommendation)"; "One package, and a panel on every node"; "Pages and REST API"

## Context

Sourced facts: the Couchbase Web Console and CLI are both clients of one REST API. VTAdmin has two components, `vtadmin-web` and `vtadmin-api`, with HTTP and gRPC. Patroni's REST API is a pattern for HA operations. etcd offers an embeddable Go server package.

**[Recommendation]** in the document: backend in Go; frontend in React with TypeScript; internal gRPC and an external REST/OpenAPI API through grpc-gateway; `kingres-cli` calls only the REST API; operations run as durable tasks with an ID and a state machine. The panel is embedded as static files in `kingresd`. The document says no source was gathered for choosing chart and topology libraries (ECharts or uPlot, React Flow) and the endpoint names are suggestions.

The repository contains `kingres/api/` for the contract. The project owner accepted an OpenAPI-first approach: the contract in `kingres/api/openapi.yaml` is written before the handlers and the client.

## Decision

The control plane is written in Go and the panel in React with TypeScript. The REST API is defined first in `kingres/api/openapi.yaml`; server handlers and the typed web client are checked against it, and the web client is generated from it. The CLI uses only the REST API.

## Consequences

- One contract serves the panel, the CLI and tests; mismatches become test failures (see `kingres/docs/dev/testing.md`).
- One language for all control-plane parts, one binary, easy embedding of etcd and the panel.
- The document describes the REST layer as grpc-gateway on top of an internal gRPC API. OpenAPI-first and grpc-gateway (which generates OpenAPI from protobuf) point in different directions: either the OpenAPI file is written by hand and gRPC is not used, or the proto files are the source and OpenAPI is generated. This ADR chooses the hand-written OpenAPI file as the source of truth for the external API; whether an internal gRPC layer exists is left open and needs its own ADR.
- Two toolchains (Go and Node) are needed, and the panel must be built before it can be embedded in the Go binary.
- Library choices (chart, topology, query library, generator) are not made here; each is recorded when chosen, with a licence check.
- The document's endpoint lists are suggestions and will change when the spec is written.
- **[Needs verification]** The document says the actual Couchbase Web Console pages, server groups, durability and the `couchbase-cli` details were not checked, and some cited REST pages were for versions 6.0/7.0; read the 8.0 documentation before implementing the matching features.

## Alternatives considered

- **Other backend languages.** Not discussed in the document beyond Erlang (Chronicle and ns_server are Erlang, and Chronicle is usable only if you use Erlang). The document names Go without a comparison.
- **gRPC-first with grpc-gateway.** The document's proposal, described above; replaced by OpenAPI-first for the external API in this ADR.
- **A server-rendered panel.** Not discussed in the document.
