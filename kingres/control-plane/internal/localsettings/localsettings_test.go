// SPDX-License-Identifier: AGPL-3.0-only

package localsettings

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

type fakeInst struct {
	mu       sync.Mutex
	s        instance.ManagedSettings
	tok      string
	writes   int
	writeErr error
}

func (f *fakeInst) Settings(int) (instance.ManagedSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

func (f *fakeInst) WriteManaged(_ int, s instance.ManagedSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.s, f.writes = s, f.writes+1
	return nil
}

func (f *fakeInst) PostmasterToken(int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok
}

func newManager(t *testing.T, fi *fakeInst, dir string) (*Manager, *slog.LevelVar) {
	t.Helper()
	var lv slog.LevelVar
	m, err := New(Options{DataDir: dir, Instances: fi, PGPort: 5433, Level: &lv,
		PhysicalMemoryMB: func() (int64, bool) { return 4096, true },
		CheckDirectory: func(p string) error {
			if p == "/missing" {
				return errors.New("does not exist")
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return m, &lv
}

func fieldsOf(t *testing.T, err error, status int) []string {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) || p.Status != status {
		t.Fatalf("want a %d problem, got %v", status, err)
	}
	var out []string
	for _, e := range p.Errors {
		out = append(out, e.Field)
	}
	return out
}

func TestQuotaOfAnInstanceCreatedBeforeItWasRecorded(t *testing.T) {
	if got := Quota(instance.ManagedSettings{Extra: map[string]string{"shared_buffers": "256MB"}}); got != 1024 {
		t.Errorf("derived from shared_buffers: %d", got)
	}
	if got := Quota(instance.ManagedSettings{MemoryMegabytes: 3000, Extra: map[string]string{"shared_buffers": "256MB"}}); got != 3000 {
		t.Errorf("recorded value wins: %d", got)
	}
	if got := Quota(instance.ManagedSettings{}); got != DefaultMemoryMB {
		t.Errorf("default: %d", got)
	}
}

func TestUpdateReplacesTheDerivedSettingsOfTheSetup(t *testing.T) {
	fi := &fakeInst{s: instance.ManagedSettings{Port: 5433, Extra: map[string]string{
		"shared_buffers": "256MB", "effective_cache_size": "512MB", "maintenance_work_mem": "64MB", "work_mem": "8MB"}}, tok: "5:1"}
	m, _ := newManager(t, fi, t.TempDir())
	got, err := m.Update([]byte(`{"memoryQuota":{"totalMegabytes":2048}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryQuota.TotalMegabytes != 2048 || !got.RestartRequired {
		t.Errorf("after: %+v", got)
	}
	if _, has := fi.s.Extra["shared_buffers"]; has || fi.s.Extra["work_mem"] != "8MB" || fi.s.MemoryMegabytes != 2048 {
		t.Errorf("managed settings: %+v", fi.s)
	}
	// The same value again writes nothing.
	w := fi.writes
	if _, err := m.Update([]byte(`{"memoryQuota":{"totalMegabytes":2048}}`)); err != nil || fi.writes != w {
		t.Errorf("an equal value must change nothing (writes %d -> %d, %v)", w, fi.writes, err)
	}
}

func TestRestartRequiredIsPersistentAndEndsWithTheRestart(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeInst{s: instance.ManagedSettings{MemoryMegabytes: 1024}, tok: "5:1"}
	m, _ := newManager(t, fi, dir)
	if _, err := m.Update([]byte(`{"logDirectory":"/var/log/x"}`)); err != nil {
		t.Fatal(err)
	}
	// A new Manager (kingresd restarted) still knows it.
	m2, _ := newManager(t, fi, dir)
	if got, _ := m2.Get(); !got.RestartRequired {
		t.Error("the pending change was forgotten by a restart of kingresd")
	}
	fi.tok = "9:2" // the instance was restarted
	if got, _ := m2.Get(); got.RestartRequired {
		t.Error("still pending after the restart")
	}
	// A change while the instance is stopped is picked up by the next start.
	fi.tok = ""
	if _, err := m2.Update([]byte(`{"memoryQuota":{"totalMegabytes":1500}}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := m2.Get(); got.RestartRequired {
		t.Error("a stopped instance needs no restart")
	}
}

func TestLogLevelIsAppliedLiveAndKept(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeInst{s: instance.ManagedSettings{MemoryMegabytes: 1024}}
	m, lv := newManager(t, fi, dir)
	got, err := m.Update([]byte(`{"logLevel":"debug"}`))
	if err != nil || got.LogLevel != "debug" || got.RestartRequired || lv.Level() != slog.LevelDebug {
		t.Fatalf("%+v %v level %v", got, err, lv.Level())
	}
	if _, err := os.Stat(filepath.Join(dir, StateFile)); err != nil {
		t.Errorf("the level is not saved: %v", err)
	}
	_, lv2 := newManager(t, fi, dir)
	if lv2.Level() != slog.LevelDebug {
		t.Errorf("the saved level is not applied at start: %v", lv2.Level())
	}
	for in, want := range map[string]string{"debug": "debug", "info": "info", "warning": "warning", "error": "error"} {
		l, ok := ParseLevel(in)
		if !ok || LevelName(l) != want {
			t.Errorf("%s: %v %v", in, l, ok)
		}
	}
	if _, ok := ParseLevel("warn"); ok {
		t.Error("the contract's name is warning, not warn")
	}
}

func TestRefusalsChangeNothing(t *testing.T) {
	fi := &fakeInst{s: instance.ManagedSettings{MemoryMegabytes: 1024}, tok: "5:1"}
	m, lv := newManager(t, fi, t.TempDir())
	for _, tc := range []struct {
		body   string
		status int
		field  string
	}{
		{`not json`, 400, "/"},
		{`[]`, 400, "/"},
		{`{"memoryQuota":{"totalMegabytes":511}}`, 400, "/memoryQuota/totalMegabytes"},
		{`{"memoryQuota":{"totalMegabytes":1.5}}`, 400, "/memoryQuota/totalMegabytes"},
		{`{"memoryQuota":5}`, 400, "/memoryQuota"},
		{`{"logDirectory":""}`, 400, "/logDirectory"},
		{`{"dataPaths":{}}`, 400, "/dataPaths"},
		{`{"memoryQuota":{"totalMegabytes":4097}}`, 422, "/memoryQuota/totalMegabytes"},
		{`{"logDirectory":"/missing"}`, 422, "/logDirectory"},
		{`{"logDirectory":"/a/../b"}`, 422, "/logDirectory"},
		{`{"logDirectory":"/a/"}`, 422, "/logDirectory"},
		// One bad member refuses the whole request: the level is not applied.
		{`{"logLevel":"debug","logDirectory":"/missing"}`, 422, "/logDirectory"},
		{`{"logLevel":"debug","memoryQuota":{"totalMegabytes":1}}`, 400, "/memoryQuota/totalMegabytes"},
	} {
		_, err := m.Update([]byte(tc.body))
		if f := fieldsOf(t, err, tc.status); len(f) == 0 || f[0] != tc.field {
			t.Errorf("%s: fields %v, want %s", tc.body, f, tc.field)
		}
	}
	if fi.writes != 0 || lv.Level() != slog.LevelInfo {
		t.Errorf("a refused update changed something: writes %d level %v", fi.writes, lv.Level())
	}
	// A failing write is an error (500), not a refusal, and changes nothing else.
	fi.writeErr = errors.New("disk full")
	if _, err := m.Update([]byte(`{"logLevel":"debug","memoryQuota":{"totalMegabytes":2048}}`)); err == nil {
		t.Fatal("a failed write was accepted")
	}
	if lv.Level() != slog.LevelInfo {
		t.Error("the level was applied although the update failed")
	}
}

// Defect 6: the optional directories are always in the answer, null when the
// server uses the default, so that a client can type them as string or null.
func TestAnswerAlwaysNamesTheOptionalDirectories(t *testing.T) {
	fi := &fakeInst{s: instance.ManagedSettings{Port: 5433}}
	m, _ := newManager(t, fi, t.TempDir())
	got, err := m.Get()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got.DataPaths)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"walDirectory", "logDirectory"} {
		if v, ok := raw[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want an explicit null", k, v, ok)
		}
	}
	fi.s.LogDirectory = "/var/log/pg"
	got, _ = m.Get()
	if got.DataPaths.LogDirectory == nil || *got.DataPaths.LogDirectory != "/var/log/pg" {
		t.Errorf("logDirectory = %v", got.DataPaths.LogDirectory)
	}
}
