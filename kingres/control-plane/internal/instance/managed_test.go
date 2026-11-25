// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	if PreloadLibraries != "kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements" {
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

// fakeInstallation is a PostgreSQL binary directory with a pg_config that names a library directory; the libraries of kingres_columnar and pg_stat_statements are in it or not (both or neither).
func fakeInstallation(t *testing.T, withOptional bool) (bin string) {
	t.Helper()
	return fakeInstallationLibs(t, withOptional, withOptional)
}

// fakeInstallationLibs is fakeInstallation with each optional library chosen.
func fakeInstallationLibs(t *testing.T, withColumnar, withStatements bool) (bin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake pg_config is a shell script")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	bin = filepath.Join(dir, "bin")
	for _, d := range []string{lib, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"kingres_shard.so", "kingres_ledger.so"} {
		if err := os.WriteFile(filepath.Join(lib, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if withColumnar {
		if err := os.WriteFile(filepath.Join(lib, "kingres_columnar.so"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if withStatements {
		if err := os.WriteFile(filepath.Join(lib, "pg_stat_statements.so"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\necho " + lib + "\n"
	for _, prog := range []string{"pg_config", "initdb", "pg_ctl"} { // the manager wants initdb and pg_ctl to exist; only pg_config is run
		body := "#!/bin/sh\nexit 1\n"
		switch prog {
		case "pg_config":
			body = script
		case "pg_ctl": // status says running while the file "running" exists (exit 3 otherwise); reload is written to pg_ctl.log
			body = "#!/bin/sh\ncase \"$1\" in\nstatus) [ -f '" + dir + "/running' ] && exit 0; exit 3;;\nreload) echo reload >> '" + dir + "/pg_ctl.log';;\nesac\n"
		}
		if err := os.WriteFile(filepath.Join(bin, prog), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func managedInstance(t *testing.T, bin string, preload string) (*Manager, string) {
	t.Helper()
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
	if err := m.WriteManaged(1, ManagedSettings{Port: 5432, Extra: map[string]string{"work_mem": "8MB"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, ConfigFileName), []byte("# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "kingres-managed.conf")
	cur, _ := os.ReadFile(path)
	// the file as an earlier package or a full list wrote it
	set := regexp.MustCompile(`(?m)^shared_preload_libraries = '[^']*'`).ReplaceAllString(string(cur), "shared_preload_libraries = '"+preload+"'")
	if err := os.WriteFile(path, []byte(set), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, path
}

// An installation without the library of kingres_columnar never gets it into the preload list: an instance whose list names a file that is not there
// cannot start (pg_ctl fails), and the next restart, a crash restart too, would take the server down. A new file and an old one are both written without it.
func TestPreloadLeavesOutALibraryThatTheInstallationLacks(t *testing.T) {
	bin := fakeInstallation(t, false)
	m, path := managedInstance(t, bin, "kingres_shard,kingres_ledger")
	if got := m.preloadList(); got != "kingres_shard,kingres_ledger" {
		t.Fatalf("preloadList = %q", got)
	}
	// the old list is already right for this installation: EnsureConfig does not add the missing library
	if changed, err := m.EnsureConfig(1); err != nil || changed {
		t.Fatalf("EnsureConfig on an old list without the library: %v %v", changed, err)
	}
	if got, _ := os.ReadFile(path); preloadOf(string(got)) != "kingres_shard,kingres_ledger" {
		t.Errorf("the list became %q", preloadOf(string(got)))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path)), columnarMarker)); err == nil {
		t.Error("an instance that never had the library has the marker")
	}
	// An instance whose file names the library has had it: it may have columnar tables, and a package upgrade that removes the old library file for a moment
	// must not take the library out of its list (the next restart would start without the resource manager and the tables could not be read). The list stays,
	// the marker is written, and every later write keeps it.
	const columnarOnly = "kingres_shard,kingres_ledger,kingres_columnar" // pg_stat_statements is not in the installation either, and is not kept
	m2, path2 := managedInstance(t, bin, columnarOnly)
	if changed, err := m2.EnsureConfig(1); err != nil || changed {
		t.Fatalf("EnsureConfig on a list that names a library that is missing now: %v %v", changed, err)
	}
	if err := m2.WriteManaged(1, ManagedSettings{Port: 5432}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path2); preloadOf(string(got)) != columnarOnly {
		t.Errorf("a later write took the library out: %q", preloadOf(string(got)))
	}
	if m2.preloadListFor(1) != columnarOnly || m2.preloadListFor(2) != "kingres_shard,kingres_ledger" {
		t.Errorf("an instance that had the library keeps it (%q), a new one does not (%q)", m2.preloadListFor(1), m2.preloadListFor(2))
	}
	// the saved settings do not keep the list the manager chose
	s, err := m2.Settings(1)
	if err != nil || s.Preload != "" {
		t.Errorf("saved settings %+v %v", s, err)
	}
}

// When the library is there, an old list is brought up to date, and the library appearing later (the package is upgraded while kingresd runs) is seen.
func TestEnsureConfigAddsTheLibraryWhenTheInstallationHasIt(t *testing.T) {
	bin := fakeInstallation(t, true)
	m, path := managedInstance(t, bin, "kingres_shard,kingres_ledger")
	changed, err := m.EnsureConfig(1)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if got, _ := os.ReadFile(path); preloadOf(string(got)) != PreloadLibraries {
		t.Errorf("the list is %q", preloadOf(string(got)))
	}
	if changed, err := m.EnsureConfig(1); err != nil || changed {
		t.Errorf("a current file was rewritten: %v %v", changed, err)
	}
}

// A running instance is reloaded when its list changes (so that PostgreSQL shows the setting as pending a restart); an instance that is not running, and a list
// that did not change, cause no reload.
func TestEnsureConfigReloadsARunningInstanceOnlyWhenTheListChanged(t *testing.T) {
	bin := fakeInstallation(t, true)
	dir := filepath.Dir(bin)
	reloads := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "pg_ctl.log"))
		return strings.Count(string(b), "reload")
	}
	m, _ := managedInstance(t, bin, "kingres_shard,kingres_ledger")
	if changed, err := m.EnsureConfig(1); err != nil || !changed || reloads() != 0 {
		t.Fatalf("not running: %v %v, %d reloads", changed, err, reloads())
	}
	// back to the old list, now running
	m2, _ := managedInstance(t, bin, "kingres_shard,kingres_ledger")
	if err := os.WriteFile(filepath.Join(dir, "running"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := m2.EnsureConfig(1); err != nil || !changed || reloads() != 1 {
		t.Fatalf("running: %v %v, %d reloads", changed, err, reloads())
	}
	if changed, err := m2.EnsureConfig(1); err != nil || changed || reloads() != 1 {
		t.Errorf("a list that did not change: %v %v, %d reloads", changed, err, reloads())
	}
}

// pg_stat_statements is in the preload list of every instance, after kingres_columnar.
func TestPreloadListNamesStatStatements(t *testing.T) {
	if !libraryIn(PreloadLibraries, "pg_stat_statements") || !strings.HasPrefix(PreloadLibraries, "kingres_shard,") {
		t.Fatalf("PreloadLibraries = %q", PreloadLibraries)
	}
	bin := fakeInstallation(t, true)
	m, _ := managedInstance(t, bin, PreloadLibraries)
	if got := m.preloadListFor(1); got != PreloadLibraries {
		t.Errorf("a full installation: %q", got)
	}
}

// An instance made before pg_stat_statements was preloaded has the list with kingres_columnar only (and its marker). EnsureConfig adds the new library,
// keeps the other settings, reloads a running instance so that PostgreSQL shows the setting as pending a restart, and does not touch a list that is current.
func TestEnsureConfigUpgradesAnExistingInstanceToStatStatements(t *testing.T) {
	bin := fakeInstallation(t, true)
	dir := filepath.Dir(bin)
	m, path := managedInstance(t, bin, "kingres_shard,kingres_ledger,kingres_columnar")
	if err := os.WriteFile(filepath.Join(dir, "running"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := m.EnsureConfig(1)
	if err != nil || !changed {
		t.Fatalf("EnsureConfig = %v, %v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if preloadOf(string(got)) != PreloadLibraries || !strings.Contains(string(got), "work_mem = '8MB'") {
		t.Errorf("the file after EnsureConfig:\n%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pg_ctl.log")); strings.Count(string(b), "reload") != 1 {
		t.Errorf("a running instance was not reloaded once: %q", b)
	}
	if changed, err := m.EnsureConfig(1); err != nil || changed {
		t.Errorf("a current file was rewritten: %v %v", changed, err)
	}
}

// An installation that has kingres_columnar but not pg_stat_statements (a package that was not upgraded) gives a list without pg_stat_statements, an
// instance that already has the columnar list is left as it is, and the library is not sticky: an instance whose list names it while the file is missing
// loses it from the list, so that the next restart cannot fail on it. When the file is back, the next EnsureConfig adds it again.
func TestStatStatementsIsLeftOutWhenTheInstallationLacksIt(t *testing.T) {
	bin := fakeInstallationLibs(t, true, false)
	const columnarOnly = "kingres_shard,kingres_ledger,kingres_columnar"
	m, path := managedInstance(t, bin, columnarOnly)
	if got := m.preloadListFor(1); got != columnarOnly {
		t.Fatalf("preloadListFor = %q", got)
	}
	if changed, err := m.EnsureConfig(1); err != nil || changed {
		t.Fatalf("EnsureConfig on a list that is right for the installation: %v %v", changed, err)
	}
	m2, path2 := managedInstance(t, bin, PreloadLibraries)
	if changed, err := m2.EnsureConfig(1); err != nil || !changed {
		t.Fatalf("EnsureConfig on a list that names a missing library: %v %v", changed, err)
	}
	if got, _ := os.ReadFile(path2); preloadOf(string(got)) != columnarOnly {
		t.Errorf("the list is %q", preloadOf(string(got)))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path2)), "pg_stat_statements")); err == nil {
		t.Error("pg_stat_statements left a marker")
	}
	// the package is repaired
	lib, _ := os.ReadFile(filepath.Join(bin, "pg_config"))
	libDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(lib)), "#!/bin/sh\necho "))
	if err := os.WriteFile(filepath.Join(libDir, "pg_stat_statements.so"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := m2.EnsureConfig(1); err != nil || !changed {
		t.Fatalf("EnsureConfig after the library came back: %v %v", changed, err)
	}
	if got, _ := os.ReadFile(path2); preloadOf(string(got)) != PreloadLibraries {
		t.Errorf("the list is %q", preloadOf(string(got)))
	}
	_ = path
}
