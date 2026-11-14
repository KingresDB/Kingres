// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The same handlers for the Node-based test runner.
import { setupServer } from 'msw/node'
import { createHandlers } from './handlers'

export const mockServer = setupServer(...createHandlers())
