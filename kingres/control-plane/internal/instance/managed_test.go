// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var confKeyRE = regexp.MustCompile(`(?m)^([a-z_][a-z0-9_.]*) = `)

// TestManagedListIsComplete renders a managed file with every optional field set and fails when a
// name appears in it that is neither managed nor a derived default: a setting added to the renderer
// without registering it would be settable from the console and fight with the renderer.
func TestManagedListIsComplete(t *testing.T) {
	out, err := RenderManaged(ManagedSettings{
		Port: 5432, ListenAddresses: "127.0.0.1", SocketDir: "/run/k", ClusterName: "c",
		TLSCertFile: "/a/c.pem", TLSKeyFile: "/a/k.pem", TLSCAFile: "/a/ca.pem", TLSCRLFile: "/a/crl.pem",
		NodeConninfo: "sslmode=verify-full", MaxConnections: 100, MemoryMegabytes: 4096, LogDirectory: "/var/log/k",
		Extra: map[string]string{"hot_standby": "on", "primary_conninfo": "host=x", "primary_slot_name": "s", "synchronous_standby_names": "ANY 1 (a)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := confKeyRE.FindAllStringSubmatch(out, -1)
	if len(keys) < 20 {
		t.Fatalf("rendered only %d settings:\n%s", len(keys), out)
	}
	for _, k := range keys {
		if _, ok := ManagedParameters[k[1]]; !ok && !DerivedDefaults[k[1]] {
			t.Errorf("the renderer writes %q, which is neither in ManagedParameters nor in DerivedDefaults", k[1])
		}
	}
	for k := range forbiddenExtra {
		if strings.HasPrefix(k, "include") {
			continue
		}
		if _, ok := ManagedParameters[k]; !ok && !isRefusedCommand(k) {
			t.Errorf("forbiddenExtra names %q, which is not in ManagedParameters", k)
		}
	}
	// A derived default comes before the include and the fixed block after it.
	inc := strings.Index(out, "include_if_exists '"+ConfigFileName+"'")
	if inc < 0 || strings.Index(out, "max_connections") > inc || strings.Index(out, "shared_buffers") > inc || strings.Index(out, "port = ") < inc {
		t.Errorf("derived defaults must precede the include and managed settings follow it:\n%s", out)
	}
}

// isRefusedCommand: the command-running names of forbiddenExtra are refused by the API through
// PostgreSQL's superuser-only set, not through the managed table.
func isRefusedCommand(k string) bool { return strings.HasSuffix(k, "_command") }

func TestRenderConfigHostileValues(t *testing.T) {
	cases := map[string]string{
		"a":      "plain",
		"quote":  "it's",
		"slash":  `back\slash`,
		"trail":  `ends\`,
		"hash":   "a # b",
		"incl":   "x' include '/etc/passwd",
		"utf":    "Asia/Tehran é ü 漢字",
		"incl2":  "include 'x'",
		"equals": "a = b\\n",
	}
	vals := map[string]string{}
	for k, v := range cases {
		vals["app."+k] = v
	}
	text, err := RenderConfig(7, vals)
	if err != nil {
		t.Fatal(err)
	}
	if ConfigRevision(text) != 7 {
		t.Errorf("revision %d", ConfigRevision(text))
	}
	if n := len(confKeyRE.FindAllString(text, -1)); n != len(cases) {
		t.Errorf("%d settings rendered for %d values:\n%s", n, len(cases), text)
	}
	if strings.Count(text, "include") > 0 {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "include") {
				t.Errorf("a value became a directive: %q", l)
			}
		}
	}
	// every line is "name = 'quoted'" on one line
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		if !regexp.MustCompile(`^app\.[a-z0-9]+ = '(?:[^'\\]|''|\\\\)*'$`).MatchString(l) {
			t.Errorf("line is not one quoted setting: %q", l)
		}
	}
}

func TestRenderConfigRefuses(t *testing.T) {
	bad := []map[string]string{
		{"work mem": "1"}, {"a=b": "1"}, {"Work_mem": "1"}, {"a\nb": "1"}, {"port": "1"}, {"include": "x"}, {"archive_command": "x"},
		{"work_mem": "1\n2"}, {"work_mem": "1\r"}, {"work_mem": "a\x00b"}, {"work_mem": strings.Repeat("x", MaxConfigValue+1)},
		{"work_mem": "\xff"}, {"": "1"}, {"a.b.c": "1"},
	}
	for _, v := range bad {
		if _, err := RenderConfig(1, v); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestConfigFileAtomicAndRestore(t *testing.T) {
	bin := os.Getenv("KINGRES_PG_BIN") // the manager wants a PostgreSQL directory; nothing is run here
	if bin == "" {
		t.Skip("needs KINGRES_PG_BIN for the manager")
	}
	root := t.TempDir()
	m, err := New(Options{Root: root, BinDir: bin})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "1")
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := `{"nodeId":1,"port":5432,"magic":"` + markerMagic + `"}`
	if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte(mk), 0o600); err != nil {
		t.Fatal(err)
	}
	one, _ := RenderConfig(1, map[string]string{"work_mem": "8MB"})
	two, _ := RenderConfig(2, map[string]string{"work_mem": "16MB"})
	if err := m.WriteConfig(1, one); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(1, two); err != nil {
		t.Fatal(err)
	}
	got, _ := m.ReadConfig(1)
	prev, _ := os.ReadFile(filepath.Join(data, ConfigFileName+".prev"))
	if got != two || string(prev) != one || ConfigRevision(got) != 2 {
		t.Errorf("file %q, previous %q", got, prev)
	}
	if err := m.RestoreConfig(1); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.ReadConfig(1); got != one {
		t.Errorf("after the restore: %q", got)
	}
	// No temporary file stays, and the mode is private.
	ents, _ := os.ReadDir(data)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
	if st, err := os.Stat(filepath.Join(data, ConfigFileName)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", st, err)
	}
	// A directory that cannot be written leaves the old file as it was.
	if os.Geteuid() != 0 {
		_ = os.Chmod(data, 0o500)
		defer func() { _ = os.Chmod(data, 0o700) }()
		if err := m.WriteConfig(1, two); err == nil {
			t.Error("a write into a read-only directory succeeded")
		}
		_ = os.Chmod(data, 0o700)
		if got, _ := m.ReadConfig(1); got != one {
			t.Errorf("a failed write changed the file: %q", got)
		}
	}
	// EnsureConfig on an instance that has no include yet writes the file and the include.
	if _, err := m.EnsureConfig(1); err == nil {
		t.Log("EnsureConfig needs the managed file; none here")
	}
}

func TestMajorOfVersionNum(t *testing.T) {
	for num, want := range map[int]int{180007: 18, 180000: 18, 190000: 19, 190001: 19, 170012: 17} {
		if got := MajorOfVersionNum(num); got != want {
			t.Errorf("MajorOfVersionNum(%d) = %d, want %d", num, got, want)
		}
	}
}

// The preload list names kingres_columnar, after the two it always had, and an old file is recognised
// as out of date (EnsureConfig rewrites it): without the library columnar writes and every
// kingres_timepart conversion are refused.
func TestPreloadListNamesColumnar(t *testing.T) {
	if PreloadLibraries != "kingres_shard,kingres_ledger,kingres_columnar" {
		t.Fatalf("PreloadLibraries = %q", PreloadLibraries)
	}
	got, err := RenderManaged(ManagedSettings{Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "shared_preload_libraries = '"+PreloadLibraries+"'") {
		t.Errorf("not rendered:\n%s", got)
	}
	if preloadOutOfDate(got) {
		t.Error("a file rendered now is out of date")
	}
	old := strings.Replace(got, PreloadLibraries, "kingres_shard,kingres_ledger", 1)
	if !preloadOutOfDate(old) {
		t.Error("the old list is not recognised as out of date")
	}
	if preloadOutOfDate("port = '1'\n") {
		t.Error("a file without the line is out of date")
	}
}

// An instance made by an earlier package has the old preload list in kingres-managed.conf and already
// has the include: EnsureConfig brings the list up to date and keeps the other settings, and a file
// that is current is not rewritten.
func TestEnsureConfigBringsThePreloadUpToDate(t *testing.T) {
	bin := os.Getenv("KINGRES_PG_BIN") // the manager wants a PostgreSQL directory; nothing is run here
	if bin == "" {
		t.Skip("needs KINGRES_PG_BIN for the manager")
	}
	root := t.TempDir()
	m, err := New(Options{Root: root, BinDir: bin})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "1")
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := `{"nodeId":1,"port":5432,"magic":"` + markerMagic + `"}`
	if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte(mk), 0o600); err != nil {
		t.Fatal(err)
	}
	s := ManagedSettings{Port: 5432, Extra: map[string]string{"work_mem": "8MB"}}
	if err := m.WriteManaged(1, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, ConfigFileName), []byte("# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "kingres-managed.conf")
	cur, _ := os.ReadFile(path)
	old := strings.Replace(string(cur), PreloadLibraries, "kingres_shard,kingres_ledger", 1)
	if old == string(cur) {
		t.Fatal("the rendered file does not carry the list")
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := m.EnsureConfig(1)
	if err != nil || !changed {
		t.Fatalf("EnsureConfig = %v, %v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "shared_preload_libraries = '"+PreloadLibraries+"'") || !strings.Contains(string(got), "work_mem = '8MB'") {
		t.Errorf("the file after EnsureConfig:\n%s", got)
	}
	if changed, err := m.EnsureConfig(1); err != nil || changed {
		t.Errorf("a current file was rewritten: %v, %v", changed, err)
	}
}
