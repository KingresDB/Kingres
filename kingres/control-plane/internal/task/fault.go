// SPDX-License-Identifier: AGPL-3.0-only

package task

// Crash points of the engine. A FaultFunc set in Options.Fault is called at
// each of them with the task id and the step index (-1 at the lease renewal,
// the number of steps at the finish points). The crash-injection tests make
// the function end the process (os.Exit) or simulate that, at one chosen
// point; nothing in the engine writes after the function returns in that
// case, which is what a killed process does.
const (
	// FaultBeforeStep is after the task was claimed (or the previous step was
	// recorded) and before step i is recorded as running.
	FaultBeforeStep = "before_step"
	// FaultStepStarted is after step i was recorded as running, before Run.
	FaultStepStarted = "step_started"
	// FaultStepRan is after Run of step i returned without error, before the
	// store records the step as done: the effect happened, the record did not.
	FaultStepRan = "step_ran"
	// FaultStepRecorded is after step i was recorded as done.
	FaultStepRecorded = "step_recorded"
	// FaultLeaseRenew is at a lease renewal tick, while a step runs.
	FaultLeaseRenew = "lease_renew"
	// FaultBeforeFinish is after the last step was recorded, before the task
	// is recorded as done.
	FaultBeforeFinish = "before_finish"
	// FaultAfterFinish is after the task was recorded as done.
	FaultAfterFinish = "after_finish"
)

// FaultPoints lists every crash point.
var FaultPoints = []string{FaultBeforeStep, FaultStepStarted, FaultStepRan, FaultStepRecorded,
	FaultLeaseRenew, FaultBeforeFinish, FaultAfterFinish}

// FaultFunc is called at a crash point (see the Fault constants).
type FaultFunc func(point string, taskID int64, step int)

func (e *Engine) fault(point string, id int64, step int) {
	if e.faultFn != nil {
		e.faultFn(point, id, step)
	}
}
