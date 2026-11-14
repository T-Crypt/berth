// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// whyDoc is one lease's story: every event berthd kept about it, oldest
// first, with the restarts it lived through, and where it stands now.
type whyDoc struct {
	Lease  string        `json:"lease" yaml:"lease"`
	Engine string        `json:"engine" yaml:"engine"`
	Card   string        `json:"card,omitempty" yaml:"card,omitempty"`
	Events []proto.Event `json:"events" yaml:"events"`
	// Now is the lease as berthd holds it now, nil once it has ended.
	Now *proto.Lease `json:"now,omitempty" yaml:"now,omitempty"`
	// Next is the lease its engine re-attached as after a berthd restart
	// cut this one off, "" when none did.
	Next string `json:"next,omitempty" yaml:"next,omitempty"`
}

// whyVerb says what each kind of event did to the lease, in the words the
// story prints. A kind not listed prints as its name.
var whyVerb = map[string]string{
	proto.EvRequested:     "asked for the card",
	proto.EvWaiting:       "waited",
	proto.EvGranted:       "granted",
	proto.EvReattached:    "re-attached after a berthd restart",
	proto.EvDraining:      "asked to give the card back",
	proto.EvReleased:      "released the card",
	proto.EvFreed:         "its memory is free",
	proto.EvRefused:       "refused",
	proto.EvOrphaned:      "its process was gone, so berthd ended the lease",
	proto.EvStuck:         "its memory stayed on the card after the release",
	proto.EvParked:        "parked in RAM",
	proto.EvResumed:       "resumed from RAM",
	proto.EvUnparked:      "loaded back onto the card without a resume",
	proto.EvParkRefused:   "could not park",
	proto.EvParkEvicted:   "stopped while parked",
	proto.EvJuggle:        "a juggle for the card",
	proto.EvQueueOverride: "the operator changed its place in line",
	proto.EvBookingPrio:   "a booking changed its priority",
	proto.EvLending:       "asked to lend part of the card",
	proto.EvLent:          "lent part of the card",
	proto.EvReturned:      "took its lent memory back",
	proto.EvLendFailed:    "the lend failed",
	proto.EvNodeStarted:   "berthd restarted",
}

// ends is the kinds after which a lease has no more to say.
var ends = []string{proto.EvFreed, proto.EvRefused, proto.EvOrphaned}

// whyOf finds the lease name means, a lease ID or else the engine's newest
// lease, and tells its story from evs. live is the leases berthd holds now.
func whyOf(evs []proto.Event, live []proto.Lease, name string) (whyDoc, bool) {
	id := ""
	for i := len(evs) - 1; i >= 0 && id == ""; i-- {
		if evs[i].Lease == name {
			id = name
		}
	}
	for i := len(evs) - 1; i >= 0 && id == ""; i-- {
		if evs[i].Engine == name && evs[i].Lease != "" {
			id = evs[i].Lease
		}
	}
	if id == "" {
		return whyDoc{}, false
	}
	doc := whyDoc{Lease: id}
	for _, l := range live {
		if l.ID == id {
			doc.Now = &l
		}
	}
	first, last := -1, -1
	for i, e := range evs {
		if e.Lease == id {
			if first < 0 {
				first = i
			}
			last = i
			doc.Engine, doc.Card = e.Engine, e.Card
		}
	}
	ended := slices.Contains(ends, evs[last].Kind)
	for i := first; i < len(evs); i++ {
		e := evs[i]
		switch {
		case e.Lease == id:
			doc.Events = append(doc.Events, e)
		case e.Kind == proto.EvNodeStarted && (i < last || (!ended && doc.Now == nil)):
			doc.Events = append(doc.Events, e)
			if i > last {
				doc.Next = reattachedAs(evs[i+1:], doc.Engine)
				return doc, true
			}
		}
	}
	return doc, true
}

// reattachedAs is the lease engine re-attached as in evs, which start just
// after a berthd restart, "" when it did not before the next restart.
func reattachedAs(evs []proto.Event, engine string) string {
	for _, e := range evs {
		if e.Kind == proto.EvNodeStarted {
			return ""
		}
		if e.Kind == proto.EvReattached && e.Engine == engine {
			return e.Lease
		}
	}
	return ""
}

type whyView struct{ doc whyDoc }

func (v whyView) Data() any { return v.doc }

func (v whyView) Human(c *Context, _ bool) error {
	d := v.doc
	head := d.Lease
	if d.Engine != "" && d.Engine != d.Lease {
		head += "  " + d.Engine
	}
	if d.Card != "" {
		head += "  " + d.Card
	}
	c.Printf("%s\n", head)
	for _, e := range d.Events {
		verb, ok := whyVerb[e.Kind]
		if !ok {
			verb = e.Kind
		}
		line := verb
		if e.Bytes != 0 {
			line += ", " + server.MiB(e.Bytes)
		}
		if e.Detail != "" {
			line += ": " + e.Detail
		}
		c.Printf("  %s  %s\n", c.Style.Dim(e.At.Local().Format("Jan 02 15:04:05")), line)
	}
	switch {
	case d.Now != nil:
		c.Printf("now %s, %s", d.Now.State, d.Now.Priority)
		if d.Now.Position > 0 {
			c.Printf(", %d in line", d.Now.Position)
		}
		if d.Now.TurnS > 0 {
			c.Printf(", %s of its minimum turn left", (time.Duration(d.Now.TurnS) * time.Second).Round(time.Second))
		}
		c.Printf("\n")
	case d.Next != "":
		c.Printf("its engine re-attached as %s: berth why %s\n", d.Next, d.Next)
	}
	return nil
}

func runWhy(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if len(c.Args) != 1 {
		return c.fail(Systemf("berth why takes one lease ID or engine name").
			WithHint("for example: berth why strata-24, or berth why comfyui"))
	}
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpEvents}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	var evs []proto.Event
	for {
		m, err := conn.Recv()
		if err != nil {
			break
		}
		if m.Event != nil {
			evs = append(evs, *m.Event)
		}
	}
	st, _ := contractStatus(*sock)
	var live []proto.Lease
	if st != nil {
		live = append(st.Leases, st.Waiting...)
	}
	doc, found := whyOf(evs, live, c.Args[0])
	if !found {
		return c.fail(Failf("no lease or engine named %q in berthd's last %d events", c.Args[0], len(evs)).
			WithHint("berth status lists the leases now; berth events lists what berthd kept"))
	}
	return c.Write(whyView{doc: doc}, nil)
}
