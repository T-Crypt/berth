// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package claims

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/T-Crypt/berth/internal/procs"
)

// Scan reads every process from /proc. Processes that exit mid-read are
// skipped; a process this user cannot read still shows its name and parent.
// It runs every two seconds over every process on the machine, so each file
// is read with one open, read and close into one buffer.
func Scan() ([]Proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(ents))
	buf := make([]byte, 0, 4096)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		var p Proc
		var ok bool
		if p, buf, ok = readProc(pid, buf); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func readProc(pid int, buf []byte) (Proc, []byte, bool) {
	st, buf, ok := procs.ReadStat(pid, buf)
	if !ok {
		return Proc{}, buf, false
	}
	p := Proc{PID: pid, Name: st.Name, Parent: st.Parent, Start: st.Start}
	// Kernel threads (kthreadd, pid 2, and its children) have no command
	// line, and a third of a desktop's processes are kernel threads.
	if pid == 2 || st.Parent == 2 {
		return p, buf, true
	}
	raw, err := procs.ReadFile("/proc/"+strconv.Itoa(pid)+"/cmdline", buf)
	if err == nil && len(raw) > 0 {
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		p.Argv0 = filepath.Base(strings.ReplaceAll(args[0], `\`, "/")) // wine argv0 can be a Windows path
		p.Cmdline = strings.Join(args, " ")
	}
	// The image path, for plain-path claim rules: /proc/pid/exe is a symlink
	// to the binary. A process that exits mid-read, or whose binary was
	// replaced, has none.
	if target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		p.Exe = target
	}
	return p, raw, true
}
