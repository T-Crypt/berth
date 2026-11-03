// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package procs

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// System reads parents from /proc.
type System struct{}

func (System) Parent(pid int) int {
	f := stat(pid)
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

// StartTime is field 22 of /proc/<pid>/stat: clock ticks since boot.
func (System) StartTime(pid int) uint64 {
	f := stat(pid)
	if len(f) < 20 {
		return 0
	}
	t, _ := strconv.ParseUint(f[19], 10, 64)
	return t
}

// List reads one /proc/<pid>/stat per process on the machine. The broker
// calls it once per tick to record engine descendants, so a child that
// escapes its wrapper before any GPU poll is still recorded under the lease.
func List() []Proc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	ps := make([]Proc, 0, len(entries))
	buf := make([]byte, 0, 512)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var p Proc
		var ok bool
		if p, buf, ok = ReadStat(pid, buf); ok {
			ps = append(ps, p)
		}
	}
	return ps
}

// Container is the container id that owns pid, from an actual
// /proc/<pid>/cgroup: docker runs its engine images in a docker-<64hex>.scope
// (v2) or a /docker/<64hex> directory (v1), podman in a libpod-<64hex>.scope,
// containerd/CRI in a cri-containerd-<64hex>.scope, and Proxmox runs an LXC
// container's processes under /lxc/<vmid>/, read as lxc:<vmid>. A host
// process reads "".
func Container(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	return containerID(bytes.NewReader(b))
}

// Scope is the Berth scope that owns pid, from an actual
// /proc/<pid>/cgroup: the path of the nearest berth-*.scope ancestor,
// "" when the process runs outside one (a plain terminal app, a cgroup
// v1 machine, any process Berth did not start).
func Scope(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	return scopeID(bytes.NewReader(b))
}

// ScopeMem reads the memory.stat of a Berth scope, a cgroup path as Scope
// returns it.
func ScopeMem(scope string) (Mem, bool) {
	b, err := os.ReadFile("/sys/fs/cgroup" + scope + "/memory.stat")
	if err != nil {
		return Mem{}, false
	}
	return scopeMem(bytes.NewReader(b))
}

// SetOOMScoreAdj sets a process's oom_score_adj and returns the value it
// had. A process may raise its own user's processes and lower them back to
// where they were without privilege.
func SetOOMScoreAdj(pid, adj int) (int, error) {
	path := "/proc/" + strconv.Itoa(pid) + "/oom_score_adj"
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	old, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return old, os.WriteFile(path, []byte(strconv.Itoa(adj)), 0)
}

// statFields reads /proc/<pid>/stat once: the command name inside the
// parenthesis, and the fields after it, so index 0 is field 3 (state). The
// name is "(comm)" and may contain spaces or parens, so the split starts
// after the last ')'.
func statFields(pid int) (string, []string) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || len(s) < i+1 {
		return "", nil
	}
	return s[1:i], strings.Fields(s[i+1:])
}

// stat returns statFields' fields.
func stat(pid int) []string {
	_, f := statFields(pid)
	return f
}
