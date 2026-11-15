// SPDX-License-Identifier: PostgreSQL

package task_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The crash-injection test of the task engine against a real PostgreSQL: the
// test binary starts itself as a helper process (TestCrashHelperProcess) that
// runs the engine on the PostgreSQL store with a Fault hook that ends the
// process with os.Exit(137) at one crash point, as a kill would. A second
// helper process then takes the task over after its lease expired. The steps
// write one line each to a side file, which is how the test counts executions
// of the non-idempotent step n.

const helperEnv = "KINGRES_CRASH_HELPER"

// helperLease is the lease of the helper processes.
const helperLease = time.Second

func helperDef(dir string, slow int) task.Definition {
	d := task.Definition{Kind: "crash.kind"}
	for i, name := range []string{"a", "n", "c", "d"} {
		d.Steps = append(d.Steps, task.Step{Name: name, NonIdempotent: name == "n", Run: func(ctx context.Context, _ *task.Env) error {
			f, err := os.OpenFile(filepath.Join(dir, "side.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(name + "\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if i == slow {
				select { // a long step: lease renewals happen meanwhile
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
			}
			return err
		}})
	}
	return d
}

// TestCrashHelperProcess is the helper process; in a normal run it returns at
// once.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	dir, point := os.Getenv("KINGRES_CRASH_DIR"), os.Getenv("KINGRES_CRASH_POINT")
	step, _ := strconv.Atoi(os.Getenv("KINGRES_CRASH_STEP"))
	slow, err := strconv.Atoi(os.Getenv("KINGRES_CRASH_SLOW"))
	if err != nil {
		slow = -1
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("KINGRES_CRASH_DSN"))
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(2)
	}
	hook := func(p string, _ int64, s int) {
		if p == point && (s == step || p == task.FaultLeaseRenew) {
			os.Exit(137)
		}
	}
	e, err := task.NewEngine(task.NewPGStore(conn), []task.Definition{helperDef(dir, slow)},
		task.Options{Holder: os.Getenv("KINGRES_CRASH_HOLDER"), Lease: helperLease, StoreTries: 1, Fault: hook})
	if err != nil {
		fmt.Println("engine:", err)
		os.Exit(2)
	}
	ran, err := e.RunOnce(ctx)
	fmt.Printf("RESULT ran=%v err=%v\n", ran, err)
	os.Exit(0)
}

func runHelper(t *testing.T, dsn, dir, holder, point string, step, slow int) (exit int, out string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "KINGRES_CRASH_DSN="+dsn, "KINGRES_CRASH_DIR="+dir,
		"KINGRES_CRASH_HOLDER="+holder, "KINGRES_CRASH_POINT="+point, "KINGRES_CRASH_STEP="+strconv.Itoa(step),
		"KINGRES_CRASH_SLOW="+strconv.Itoa(slow))
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, buf.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), buf.String()
	default:
		t.Fatal(err)
		return -1, ""
	}
}

func sideCounts(t *testing.T, dir string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "side.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	m := map[string]int{}
	for _, l := range strings.Fields(string(b)) {
		m[l]++
	}
	return m
}

func TestTaskEngineSurvivesAKilledProcess(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	conn := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	dsn := in.DSN("kingres_system")
	sub, err := task.NewEngine(task.NewPGStore(conn), []task.Definition{helperDef("", -1)}, task.Options{Holder: "test"})
	if err != nil {
		t.Fatal(err)
	}

	type tc struct {
		point string
		step  int
		slow  int
	}
	var cases []tc
	for _, p := range []string{task.FaultBeforeStep, task.FaultStepStarted, task.FaultStepRan, task.FaultStepRecorded} {
		for k := 0; k < 4; k += 1 {
			if k == 3 && p != task.FaultStepRan { // the last step adds nothing new
				continue
			}
			cases = append(cases, tc{p, k, -1})
		}
	}
	cases = append(cases, tc{task.FaultLeaseRenew, 0, 0}, tc{task.FaultLeaseRenew, 1, 1}, tc{task.FaultLeaseRenew, 2, 2},
		tc{task.FaultBeforeFinish, 4, -1}, tc{task.FaultAfterFinish, 4, -1})

	for n, c := range cases {
		t.Run(fmt.Sprintf("%s#%d", c.point, c.step), func(t *testing.T) {
			dir := t.TempDir()
			tk, err := sub.Submit(ctx, "crash.kind", task.Subject{Type: "crash", ID: strconv.Itoa(n)}, nil, "test")
			if err != nil {
				t.Fatal(err)
			}
			code, out := runHelper(t, dsn, dir, "A", c.point, c.step, c.slow)
			if code != 137 {
				t.Fatalf("helper A exited %d, want the injected 137:\n%s", code, out)
			}
			mid, _ := sub.Get(ctx, tk.ID)
			if c.point == task.FaultAfterFinish {
				if mid.State != task.Done {
					t.Fatalf("after the finish the row is %s", mid.State)
				}
			} else if mid.State != task.Running || mid.Holder != "A" {
				t.Fatalf("after the kill the row is %s held by %q, want running with a lease", mid.State, mid.Holder)
			}
			time.Sleep(helperLease + 300*time.Millisecond)
			code, out = runHelper(t, dsn, dir, "B", "", 0, -1)
			if code != 0 {
				t.Fatalf("helper B exited %d:\n%s", code, out)
			}
			wantRan := c.point != task.FaultAfterFinish
			if got := strings.Contains(out, "ran=true"); got != wantRan {
				t.Errorf("B ran a task: %v, want %v\n%s", got, wantRan, out)
			}
			got, _ := sub.Get(ctx, tk.ID)
			counts := sideCounts(t, dir)
			interrupted := c.step == 1 && (c.point == task.FaultStepStarted || c.point == task.FaultStepRan || c.point == task.FaultLeaseRenew)
			if interrupted {
				if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeStepInterrupted {
					t.Fatalf("want a clean step_interrupted failure, got %s %+v", got.State, got.Error)
				}
				if got.Holder != "" {
					t.Errorf("the failed task still has a lease holder %q", got.Holder)
				}
			} else if got.State != task.Done {
				t.Fatalf("want done, got %s %+v (side file %v)", got.State, got.Error, counts)
			}
			if counts["n"] > 1 {
				t.Errorf("the non-idempotent step ran %d times", counts["n"])
			}
			for i, name := range []string{"a", "n", "c", "d"} {
				max := 1
				if (c.point == task.FaultStepRan && i == c.step) || (c.point == task.FaultLeaseRenew && i == c.step && name != "n") {
					max = 2 // its effect happened but was not recorded: at-least-once
				}
				if counts[name] > max {
					t.Errorf("step %s ran %d times (max %d), side file %v", name, counts[name], max, counts)
				}
				if !interrupted && counts[name] < 1 {
					t.Errorf("step %s never ran, side file %v", name, counts)
				}
			}
		})
	}
}
