// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package eventlog is berthd's event stream: one numbered sequence for the
// whole machine, kept in a ring and fanned out to watchers. Every card's
// broker on a node writes to the same log, so `berth events` is one ordered
// stream however many cards the node governs. Opened on a file, the stream
// outlives berthd: a restart reads the last Ring events back and numbers on
// from them.
package eventlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/T-Crypt/berth/internal/proto"
)

// Ring is how many events the log keeps in memory.
const Ring = 1000

// watchBuffer is how far a watcher may fall behind before its channel is
// closed.
const watchBuffer = 256

// maxFile is how large the event file grows before it is moved to
// path.1, which keeps one file of history behind the live one.
const maxFile = 8 << 20

// Log is one node's event stream.
type Log struct {
	mu     sync.Mutex
	seq    uint64
	events []proto.Event // ring, oldest first
	subs   map[chan proto.Event]struct{}
	// file is where each event is appended, nil for a log kept in memory
	// only or one whose file failed a write.
	file    *os.File
	path    string
	size    int64
	maxFile int64
}

// New returns an empty log kept in memory only.
func New() *Log { return &Log{subs: map[chan proto.Event]struct{}{}} }

// Open returns the log kept in path: the newest Ring events in path.1 and
// path come back, numbering goes on after the newest, and each new event is
// appended. A line a crash cut short is skipped.
func Open(path string) (*Log, error) {
	l := New()
	for _, p := range []string{path + ".1", path} {
		if err := l.load(p); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	l.file, l.path, l.size, l.maxFile = f, path, fi.Size(), maxFile
	if torn(path, fi.Size()) {
		n, _ := f.Write([]byte{'\n'})
		l.size += int64(n)
	}
	return l, nil
}

// torn is whether the file's last line has no newline: a crash cut it, and
// the next event must start a line of its own.
func torn(path string, size int64) bool {
	if size == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 1)
	_, err = f.ReadAt(b, size-1)
	return err == nil && b[0] != '\n'
}

func (l *Log) load(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e proto.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Seq <= l.seq {
			continue
		}
		l.seq = e.Seq
		l.keep(e)
	}
	return sc.Err()
}

// Close stops writing to the file. The log keeps working in memory.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// LastLease is the highest lease number the kept events name, so a
// restarted berthd does not hand out lease IDs its history already uses.
func (l *Log) LastLease() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n uint64
	for _, e := range l.events {
		i := strings.LastIndexByte(e.Lease, '-')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseUint(e.Lease[i+1:], 10, 64); err == nil && v > n {
			n = v
		}
	}
	return n
}

func (l *Log) keep(e proto.Event) {
	if len(l.events) == Ring {
		copy(l.events, l.events[1:])
		l.events = l.events[:Ring-1]
	}
	l.events = append(l.events, e)
}

// write appends e to the file, moving the file aside first when it is
// full. A failed write ends the file: the stream goes on in memory.
func (l *Log) write(e proto.Event) {
	if l.file == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if l.size+int64(len(line)) > l.maxFile && l.size > 0 {
		l.file.Close()
		l.file = nil
		if os.Rename(l.path, l.path+".1") != nil {
			return
		}
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		l.file, l.size = f, 0
	}
	n, err := l.file.Write(line)
	l.size += int64(n)
	if err != nil {
		l.file.Close()
		l.file = nil
	}
}

// Emit numbers e, keeps it, and hands it to every watcher. A watcher whose
// buffer is full is dropped, so one slow client never stalls a broker.
func (l *Log) Emit(e proto.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	l.keep(e)
	l.write(e)
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(l.subs, ch)
		}
	}
}

// Last is the sequence number of the newest event, 0 before the first.
func (l *Log) Last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Events returns the events after since, and, if watch, a channel of every
// later one plus a func to stop watching. The channel closes if the watcher
// falls more than 256 events behind.
func (l *Log) Events(since uint64, watch bool) ([]proto.Event, <-chan proto.Event, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []proto.Event
	for _, e := range l.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	if !watch {
		return out, nil, func() {}
	}
	ch := make(chan proto.Event, watchBuffer)
	l.subs[ch] = struct{}{}
	return out, ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.subs[ch]; ok {
			delete(l.subs, ch)
			close(ch)
		}
	}
}
