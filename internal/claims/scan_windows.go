// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claims

import (
	"strings"

	"golang.org/x/sys/windows"

	"github.com/T-Crypt/berth/internal/procs"
)

// Scan reads the Toolhelp32 process table. Windows gives no other process's
// command line without reading its memory, so Cmdline holds the executable's
// full path instead: cmdline rules match install folders
// (\steamapps\common\, \Epic Games\), which is where games live.
func Scan() ([]Proc, error) {
	var out []Proc
	sys := procs.System{}
	err := procs.Walk(func(e *windows.ProcessEntry32) {
		pid := int(e.ProcessID)
		if pid == 0 {
			return
		}
		p := Proc{PID: pid, Parent: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
		p.Argv0 = p.Name
		p.Cmdline = imagePath(pid)
		p.Exe = p.Cmdline // on Windows the command line read is the image path
		p.Start = sys.StartTime(pid)
		out = append(out, p)
	})
	return out, err
}

func imagePath(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return strings.TrimSpace(windows.UTF16ToString(buf[:n]))
}
