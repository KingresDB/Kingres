// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

// SlotAction is what the slot policy asks for.
type SlotAction int

// Slot actions.
const (
	SlotOK   SlotAction = iota // the slot retains little WAL
	SlotWarn                   // it retains at least warn bytes: raise the alert
	SlotDrop                   // it retains more than limit bytes: drop it, rebuild the standby
)

// SlotPolicy judges the WAL a physical slot retains. A dead standby must not be
// able to fill the primary's disk: an alert comes first, and the slot is dropped
// when the limit is exceeded (the standby is then rebuilt from a new base backup
// when it returns).
func SlotPolicy(retainedBytes, warn, limit int64) SlotAction {
	switch {
	case retainedBytes > limit:
		return SlotDrop
	case retainedBytes >= warn:
		return SlotWarn
	default:
		return SlotOK
	}
}
