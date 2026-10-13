package proto_test

import (
	"bytes"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// A writer without deadline support sends exactly like Send.
func TestSendWithDeadlinePlainWriterMatchesSend(t *testing.T) {
	var a, b bytes.Buffer
	m := proto.Msg{Op: proto.OpStatus}
	if err := proto.NewConn(rw(&a)).SendWithDeadline(m, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := proto.NewConn(rw(&b)).Send(m); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() || a.Len() == 0 {
		t.Fatalf("deadline send differs from Send: %q vs %q", a.String(), b.String())
	}
}

// A reader that never reads trips the deadline instead of wedging the
// writer: the error is a timeout, and it arrives near the deadline.
func TestSendWithDeadlineTimesOutOnSlowReader(t *testing.T) {
	r, w := net.Pipe()
	defer r.Close()
	defer w.Close()
	c := proto.NewConn(w)
	start := time.Now()
	err := c.SendWithDeadline(proto.Msg{Op: proto.OpEvent}, 200*time.Millisecond)
	el := time.Since(start)
	if err == nil {
		t.Fatal("slow reader was never dropped: SendWithDeadline succeeded")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if el > 5*time.Second {
		t.Fatalf("slow reader wedged the writer for %s", el)
	}
}

type rwBuf struct{ *bytes.Buffer }

func rw(b *bytes.Buffer) *rwBuf { return &rwBuf{b} }

func (r *rwBuf) Read(p []byte) (int, error) { return 0, os.ErrClosed }
