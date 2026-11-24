// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sysdb

// AlertExtensionSkipped is the kind of the alert that says an extension of a database was left out on a server because a role of the tenant made a schema of the
// extension its own (contract 0.30.0). It is raised by the cluster service when it applies a database to the server (subject "server:<id>|database:<name>"),
// and cleared when a later apply of that extension on that server succeeds or the database is dropped. The monitor does not manage it.
const AlertExtensionSkipped = "extensionSkipped"
