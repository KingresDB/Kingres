// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task

// EnvFor returns the Env of a step that is called directly, without an engine: the tests of the steps of a
// task kind run a step again with the row of a task that ran. Its Fence is 0 and SetDetail records nothing.
func EnvFor(t Task) *Env { return &Env{Task: t} }
