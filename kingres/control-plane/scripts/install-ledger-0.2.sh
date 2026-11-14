#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copies the kingres_ledger extension files (control, SQL scripts, library) of
# the installation in ~/kw-ledger/install into the private installation
# ~/kw-cp/install, so that the tests run against ledger 0.2. Nothing else is copied.
set -euo pipefail
S="$HOME/kw-ledger/install"; D="$HOME/kw-cp/install"
cp "$S"/share/postgresql/extension/kingres_ledger* "$D/share/postgresql/extension/"
cp "$S/lib/x86_64-linux-gnu/postgresql/kingres_ledger.so" "$D/lib/x86_64-linux-gnu/postgresql/"
grep default_version "$D/share/postgresql/extension/kingres_ledger.control"
