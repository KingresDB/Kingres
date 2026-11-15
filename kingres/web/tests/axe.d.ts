// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import 'vitest'

declare module 'vitest' {
  // The matcher is registered in tests/setup.ts with vitest-axe.
  interface Assertion {
    toHaveNoViolations(): void
  }
  interface AsymmetricMatchersContaining {
    toHaveNoViolations(): void
  }
}
