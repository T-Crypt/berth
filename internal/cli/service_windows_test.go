// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package cli

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderTaskXML(t *testing.T) {
	u := Unit{
		Name:        "berthd",
		Exec:        []string{`C:\berth\berth.exe`, "daemon", "--gpu", "0", "--poll", "2s"},
		Description: "berth - GPU lease broker (every card, many inference engines)",
	}
	doc, err := renderTaskXML(u, `ACME\trevin`)
	if err != nil {
		t.Fatal(err)
	}
	// The token walk is the well-formedness check.
	// The rendering is UTF-8 in memory; only the file schtasks reads is
	// UTF-16, so the declaration's charset is read through as is.
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("the task XML is not well-formed: %v", err)
		}
	}
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		`<LogonTrigger>`,
		`<Enabled>true</Enabled>`,
		`<UserId>ACME\trevin</UserId>`,
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<StartWhenAvailable>true</StartWhenAvailable>`,
		`<Hidden>true</Hidden>`,
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<RestartOnFailure>`,
		`<Interval>PT1M</Interval>`,
		`<Count>3</Count>`,
		`<Command>C:\berth\berth.exe</Command>`,
		`<Arguments>daemon --gpu 0 --poll 2s</Arguments>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the task XML lacks %s:\n%s", want, doc)
		}
	}
	if strings.Count(doc, `<UserId>`) != 2 {
		t.Errorf("the user is named in the trigger and the principal:\n%s", doc)
	}

	escaped, err := renderTaskXML(Unit{Name: "x", Exec: []string{"c"}, Description: "a & b <c>"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(escaped, "<Description>a &amp; b &lt;c&gt;</Description>") {
		t.Errorf("the description is XML-escaped:\n%s", escaped)
	}
	if strings.Contains(escaped, "<UserId>") {
		t.Errorf("no user is named when it is unknown:\n%s", escaped)
	}
}

func TestUtf16le(t *testing.T) {
	cases := map[string]string{
		"AB":    "AB",
		"héllo": "héllo",
	}
	for in, want := range cases {
		dec := utf16decode(utf16le(in))
		if dec != want {
			t.Errorf("utf16le round trip of %q = %q", in, dec)
		}
	}
	// A supplementary-plane character is a surrogate pair in UTF-16.
	if got := utf16decode(utf16le("旗\U0001F680")); got != "旗\U0001F680" {
		t.Errorf("the surrogate pair round-trips, got %q", got)
	}
	b := utf16le("A")
	if len(b) != 4 || b[0] != 0xff || b[1] != 0xfe || b[2] != 'A' || b[3] != 0 {
		t.Errorf("the BOM and little-endian form, got % x", b)
	}
}

func utf16decode(b []byte) string {
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe {
		return ""
	}
	var s strings.Builder
	for i := 2; i+1 < len(b); i += 2 {
		r := rune(b[i]) | rune(b[i+1])<<8
		if r >= 0xd800 && r <= 0xdbff && i+3 < len(b) {
			r2 := rune(b[i+2]) | rune(b[i+3])<<8
			s.WriteRune(0x10000 + (r-0xd800)<<10 + (r2 - 0xdc00))
			i += 2
		} else {
			s.WriteRune(r)
		}
	}
	return s.String()
}

func TestCsvFields(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`"\Berth\berthd","Running","12:00:00"`, []string{`\Berth\berthd`, "Running", "12:00:00"}},
		{`"a ""quoted"" b",Ready`, []string{`a "quoted" b`, "Ready"}},
		{`plain,Running`, []string{"plain", "Running"}},
	}
	for _, c := range cases {
		got, err := csvFields(c.line)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("csvFields(%q) = %v, %v; want %v", c.line, got, err, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("csvFields(%q) = %v; want %v", c.line, got, c.want)
			}
		}
	}
}

func TestTaskNotFound(t *testing.T) {
	for _, out := range []string{
		`ERROR: The system cannot find the file specified.`,
		`ERROR: The task is not found.`,
		`The task does not exist`,
	} {
		if !taskNotFound(out) {
			t.Errorf("taskNotFound(%q) = false", out)
		}
	}
	if taskNotFound(`ERROR: Access is denied.`) {
		t.Error("an access failure is not a missing task")
	}
}

func TestServiceInstallWindows(t *testing.T) {
	dir := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const exe = `C:\berth\berth.exe`
	// Each /Create's XML is read while schtasks would read it: install
	// removes the file once the task exists. A task is not there yet, so
	// the /Query for a backup comes back not-found.
	var xmlFiles []string
	var xmlTexts []string
	var runs []argv
	ops, calls := fakeOps(t, dir, func(a argv) (string, error) {
		switch a.args[0] {
		case "/Query":
			return `ERROR: The system cannot find the file specified.`, errors.New("exit status 1")
		case "/Create":
			b, err := os.ReadFile(a.args[4])
			if err != nil {
				t.Errorf("the XML file schtasks was given: %v", err)
			}
			xmlFiles = append(xmlFiles, a.args[4])
			xmlTexts = append(xmlTexts, utf16decode(b))
		case "/Run":
			runs = append(runs, a)
		}
		return "", nil
	})

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(exe, true, false), false)
	if e != nil {
		t.Fatalf("install failed: %v", e)
	}
	if len(*calls) != 6 {
		t.Fatalf("a query and a create per task, then a run per task, got %v", *calls)
	}
	if got := (*calls)[0].String(); got != `schtasks.exe /Query /TN \Berth\berthd /XML` {
		t.Errorf("the differing task is asked for first, got %s", got)
	}
	if got := (*calls)[1].String(); got != "schtasks.exe /Create /TN \\Berth\\berthd /XML "+xmlFiles[0]+" /F" {
		t.Errorf("the create command, got %s", got)
	}
	if got := (*calls)[4].String(); got != `schtasks.exe /Run /TN \Berth\berthd` {
		t.Errorf("the task is started after creating, got %s", got)
	}
	if len(runs) != 2 {
		t.Fatalf("one run per task, got %v", runs)
	}
	if len(xmlFiles) != 2 {
		t.Fatalf("one XML per task, got %v", xmlFiles)
	}
	logPath := filepath.Join(state, "berth", "logs", "berthd.log")
	stubPath := filepath.Join(state, "berth", "units", "berthd.vbs")
	text := xmlTexts[0]
	// A console program as the action opens a window at every logon
	// (issue 165): the task runs a wscript stub, which opens none.
	for _, want := range []string{
		`<Command>wscript.exe</Command>`,
		`<Arguments>//B //Nologo &quot;` + stubPath + `&quot;</Arguments>`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the task's action is the unit's hidden stub, want %s:\n%s", want, text)
		}
	}
	b, err := os.ReadFile(stubPath)
	if err != nil {
		t.Fatalf("the stub is written before the task: %v", err)
	}
	stub := utf16decode(b)
	wantRun := `rc = sh.Run("cmd.exe /c """"` + exe + `"" daemon --gpu 0 --poll 2s >> ""` + logPath + `"" 2>&1""", 0, True)`
	for _, want := range []string{`Set sh = CreateObject("WScript.Shell")`, wantRun, `WScript.Quit rc`} {
		if !strings.Contains(stub, want) {
			t.Errorf("the stub runs the user's binary hidden with its output on the log, want %s:\n%s", want, stub)
		}
	}
	if strings.Contains(text, "berth-gateway") {
		t.Errorf("each task names its own unit:\n%s", text)
	}
	if fi, err := os.Stat(filepath.Join(state, "berth", "logs")); err != nil || !fi.IsDir() {
		t.Errorf("the log directory exists before the first start: %v", err)
	}
	if _, err := os.Stat(xmlFiles[0]); !os.IsNotExist(err) {
		t.Errorf("the XML file is deleted after schtasks read it: %v", err)
	}
	if doc.Units[0].Action != "created, started" {
		t.Errorf("the task is created and started, got %+v", doc.Units[0])
	}
}

func TestServiceInstallWindowsCommandFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		return "ERROR: Access is denied.", errors.New("exit status 1")
	})

	_, e := serviceInstall(context.Background(), ops, serviceUnits(`C:\berth\berth.exe`, false, false), false)
	if e == nil || e.Code != ExitSystem {
		t.Errorf("a failing schtasks is a system failure, got %+v", e)
	}
	if !strings.Contains(e.Msg, "Access is denied") {
		t.Errorf("the failure carries the command's output, got %q", e.Msg)
	}
}

// A task already in the folder whose XML differs from what berth would
// write is kept in the state directory as .bak before /F replaces it, like
// the systemd path keeps a differing unit file; a task that matches what
// berth writes is replaced with no copy.
func TestServiceInstallWindowsBacksUpDifferingTask(t *testing.T) {
	dir := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const exe = `C:\berth\berth.exe`
	units := serviceUnits(exe, true, true)
	same, err := unitTaskXML(units[0], currentUser())
	if err != nil {
		t.Fatal(err)
	}
	const custom = `<?xml version="1.0"?><Task>a task the operator wrote</Task>`
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		if a.args[0] == "/Query" {
			if a.args[2] == `\Berth\berthd` {
				return same, nil
			}
			return custom, nil
		}
		return "", nil
	})

	doc, e := serviceInstall(context.Background(), ops, units, false)
	if e != nil {
		t.Fatalf("install failed: %v", e)
	}
	byName := map[string]serviceUnit{}
	for _, u := range doc.Units {
		byName[u.Unit] = u
	}
	if byName["berthd"].BackedUp != "" {
		t.Errorf("a task berth would write the same is replaced with no copy, got %+v", byName["berthd"])
	}
	want := filepath.Join(state, "berth", "berth-harbor.xml.bak")
	if byName["berth-harbor"].BackedUp != want {
		t.Errorf("the replaced task is kept as .bak, got %+v", byName["berth-harbor"])
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("the previous copy is kept: %v", err)
	}
	if string(b) != custom {
		t.Errorf("the copy is the task schtasks exported, got %q", b)
	}
}

// A start the scheduler refuses is reported on the row it belongs to: the
// task exists and starts at the next logon, so the install is not a
// failure.
func TestServiceInstallWindowsReportsAFailingStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		switch a.args[0] {
		case "/Query":
			return `ERROR: The system cannot find the file specified.`, errors.New("exit status 1")
		case "/Run":
			return "ERROR: (2,2):The system cannot find the file specified.", errors.New("exit status 2")
		}
		return "", nil
	})

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(`C:\berth\berth.exe`, false, false), false)
	if e != nil {
		t.Fatalf("a refused start does not fail the install: %v", e)
	}
	for _, want := range []string{"created, not started", "The system cannot find the file specified"} {
		if !strings.Contains(doc.Units[0].Action, want) {
			t.Errorf("the failed start is on the row, got %q", doc.Units[0].Action)
		}
	}
}

func TestServiceInstallWindowsDryRun(t *testing.T) {
	dir := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	ops, calls := fakeOps(t, dir, nil)

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(`C:\berth\berth.exe`, true, false), true)
	if e != nil {
		t.Fatalf("dry run failed: %v", e)
	}
	if len(*calls) != 0 {
		t.Errorf("a dry run runs no command, got %v", *calls)
	}
	for _, u := range doc.Units {
		if u.Action != "would be created and started" || u.Content == "" {
			t.Errorf("a dry run shows the task it would write, got %+v", u)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "berth", "logs")); !os.IsNotExist(err) {
		t.Errorf("a dry run creates no log directory: %v", err)
	}
}

func TestServiceStatusWindows(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	ops, _ := fakeOps(t, t.TempDir(), func(a argv) (string, error) {
		if a.args[0] == "/Query" {
			switch a.args[2] {
			case `\Berth\berthd`:
				// schtasks /FO CSV /NH prints TaskName, Next Run Time,
				// Status; a logon task has no next run time (issue 166).
				return `"\Berth\berthd","N/A","Running"`, nil
			case `\Berth\berth-gateway`:
				return `ERROR: The system cannot find the file specified.`, errors.New("exit status 1")
			case `\Berth\berth-harbor`:
				return `"\Berth\berth-harbor","N/A","Ready"`, nil
			}
		}
		return "", errors.New("unexpected call " + a.String())
	})

	doc, e := serviceStatus(context.Background(), ops, allUnits())
	if e != nil {
		t.Fatalf("status failed: %v", e)
	}
	byName := map[string]serviceUnit{}
	for _, u := range doc.Units {
		byName[u.Unit] = u
	}
	if !byName["berthd"].Installed || !byName["berthd"].Running {
		t.Errorf("berthd is installed and running, got %+v", byName["berthd"])
	}
	if want := filepath.Join(state, "berth", "logs", "berthd.log"); byName["berthd"].Log != want {
		t.Errorf("status names the task's log file, got %+v", byName["berthd"])
	}
	if byName["berth-gateway"].Installed {
		t.Errorf("a task schtasks cannot find is not installed, got %+v", byName["berth-gateway"])
	}
	if byName["berth-gateway"].Log != "" {
		t.Errorf("a missing task names no log, got %+v", byName["berth-gateway"])
	}
	if !byName["berth-harbor"].Installed || byName["berth-harbor"].Running {
		t.Errorf("berth-harbor is installed and ready, not running, got %+v", byName["berth-harbor"])
	}
}

func TestServiceUninstallWindows(t *testing.T) {
	// The uninstall removes each unit's stub for real: without its own
	// state directory it deletes a live install's launchers (issue 351).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ops, calls := fakeOps(t, t.TempDir(), func(a argv) (string, error) {
		if a.args[0] == "/Delete" {
			if a.args[2] == `\Berth\berth-gateway` {
				return `ERROR: The system cannot find the file specified.`, errors.New("exit status 1")
			}
		}
		return "", nil
	})

	doc, e := serviceUninstall(context.Background(), ops, allUnits(), false)
	if e != nil {
		t.Fatalf("uninstall failed: %v", e)
	}
	byName := map[string]serviceUnit{}
	for _, u := range doc.Units {
		byName[u.Unit] = u
	}
	if byName["berthd"].Action != "deleted" || byName["berth-harbor"].Action != "deleted" {
		t.Errorf("found tasks are deleted, got %+v", doc.Units)
	}
	if byName["berth-gateway"].Action != "not installed" {
		t.Errorf("a missing task is reported, not an error, got %+v", byName["berth-gateway"])
	}
	var got []string
	for _, c := range *calls {
		got = append(got, c.String())
	}
	want := []string{
		`schtasks.exe /End /TN \Berth\berthd`, `schtasks.exe /Delete /TN \Berth\berthd /F`,
		`schtasks.exe /End /TN \Berth\berth-gateway`, `schtasks.exe /Delete /TN \Berth\berth-gateway /F`,
		`schtasks.exe /End /TN \Berth\berth-harbor`, `schtasks.exe /Delete /TN \Berth\berth-harbor /F`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("each task is ended before it is deleted, got %v", got)
	}
}

// A task that is not running makes /End fail; the uninstall still deletes it.
func TestServiceUninstallWindowsIgnoresEndFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ops, _ := fakeOps(t, t.TempDir(), func(a argv) (string, error) {
		if a.args[0] == "/End" {
			return "ERROR: There is no running instance of the task.", errors.New("exit status 1")
		}
		return "", nil
	})
	doc, e := serviceUninstall(context.Background(), ops, allUnits(), false)
	if e != nil {
		t.Fatalf("a task that is not running does not fail the uninstall: %v", e)
	}
	for _, u := range doc.Units {
		if u.Action != "deleted" {
			t.Errorf("the task is deleted, got %+v", u)
		}
	}
}

// A task that already existed is ended before /Run: its XML ignores a new
// instance while the old one runs.
func TestServiceInstallWindowsRestartsExistingTask(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	units := serviceUnits(`C:\berth\berth.exe`, false, false)
	same, err := unitTaskXML(units[0], currentUser())
	if err != nil {
		t.Fatal(err)
	}
	ops, calls := fakeOps(t, t.TempDir(), func(a argv) (string, error) {
		if a.args[0] == "/Query" {
			return same, nil
		}
		return "", nil
	})
	if _, e := serviceInstall(context.Background(), ops, units, false); e != nil {
		t.Fatalf("install failed: %v", e)
	}
	n := len(*calls)
	if n < 3 {
		t.Fatalf("too few calls: %v", *calls)
	}
	end, run := (*calls)[n-2].String(), (*calls)[n-1].String()
	if end != `schtasks.exe /End /TN \Berth\berthd` || run != `schtasks.exe /Run /TN \Berth\berthd` {
		t.Errorf("the existing task is ended, then run, got %q then %q", end, run)
	}
}
