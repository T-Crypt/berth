package broker

// Juggles. Two leases at the same priority drain each other ("last switch
// wins"), which is right for a person switching models and wrong when two
// clients ask for different engines at once: the card flips back and forth
// and every client fails. A drain of a holder granted less than JuggleWindow
// ago, by a waiter at the same priority, is a juggle. The engine order
// decides it (Config.Order, else the engine with the most card-hours through
// Berth over the last 7 days, else the holder keeps the card), and the verdict
// stands for JuggleWindow: the loser's engine waits instead of draining the
// winner, and a waiter that lost keeps waiting until the winner releases or
// its own wait runs out. Outside a juggle, last switch wins as before.

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

const (
	// DefaultJuggleWindow is how soon after a grant an equal-priority drain
	// counts as a juggle.
	DefaultJuggleWindow = 15 * time.Second
	usageDays           = 7
)

// verdict is one decided juggle: the loser's engine does not drain the
// winning lease at equal priority until the verdict expires.
type verdict struct {
	winner string // lease id
	until  time.Time
	why    string
}

// key is what the order and the usage hours are kept by: the lease's group
// (llama-swap runs every model under its own name, all in group
// "llama-swap"), else its engine name.
func (l *lease) key() string {
	if l.group != "" {
		return l.group
	}
	return l.engine
}

func (b *Broker) juggleWindow() time.Duration {
	if b.cfg.JuggleWindow < 0 {
		return 0 // off: last switch always wins
	}
	if b.cfg.JuggleWindow == 0 {
		return DefaultJuggleWindow
	}
	return b.cfg.JuggleWindow
}

// outranks says whether engine a beats engine b in a juggle, and why. A tie
// is not a win: the holder keeps the card.
func (b *Broker) outranks(a, bk string, now time.Time) (bool, string) {
	if len(b.cfg.Order) > 0 {
		ia, ib := orderIndex(b.cfg.Order, a), orderIndex(b.cfg.Order, bk)
		if ia != ib {
			return ia < ib, fmt.Sprintf("order: %s", strings.Join(b.cfg.Order, " > "))
		}
	}
	ha, hb := b.usageHours(a, now), b.usageHours(bk, now)
	if ha != hb {
		return ha > hb, fmt.Sprintf("usage: %s %.1f h vs %s %.1f h on the card in %d days", a, ha, bk, hb, usageDays)
	}
	return false, "a tie; the holder keeps the card"
}

func orderIndex(order []string, k string) int {
	if i := slices.Index(order, k); i >= 0 {
		return i
	}
	return len(order) // unlisted engines rank after every listed one
}

// keeps decides an equal-priority waiter w against holder h: true means h
// keeps the card and w waits, with the reason w is told.
func (b *Broker) keeps(h, w *lease, now time.Time) (bool, string) {
	if w.lostTo == h.id {
		return true, w.lostWhy
	}
	if v, ok := b.verdicts[w.key()]; ok && v.winner == h.id && now.Before(v.until) {
		w.lostTo, w.lostWhy = h.id, v.why
		return true, v.why
	}
	win := b.juggleWindow()
	if win == 0 || h.granted.IsZero() || h.reattach || now.Sub(h.granted) >= win {
		return false, "" // a switch, not a juggle: last switch wins
	}
	if b.cfg.Busy != nil {
		if busy, known := b.cfg.Busy(h.key()); known && !busy {
			return false, "" // the holder is idle: handing over costs nobody a request
		}
	}
	wins, why := b.outranks(w.key(), h.key(), now)
	if wins {
		msg := fmt.Sprintf("juggle: %s takes the card from %s (%s)", w.key(), h.key(), why)
		b.verdicts[h.key()] = verdict{winner: w.id, until: now.Add(win), why: msg}
		b.counters.JugglesStopped++
		b.log.Info("juggle", "winner", w.id, "loser", h.id, "why", why)
		b.emit(proto.Event{Kind: proto.EvJuggle, Lease: w.id, Engine: w.engine, Detail: msg})
		return false, ""
	}
	msg := fmt.Sprintf("juggle: %s keeps the card over %s (%s)", h.key(), w.key(), why)
	b.verdicts[w.key()] = verdict{winner: h.id, until: now.Add(win), why: msg}
	w.lostTo, w.lostWhy = h.id, msg
	b.counters.JugglesStopped++
	b.log.Info("juggle", "winner", h.id, "loser", w.id, "why", why)
	b.emit(proto.Event{Kind: proto.EvJuggle, Lease: h.id, Engine: h.engine, Detail: msg})
	return true, msg
}

// pruneVerdicts drops verdicts that expired or whose winner is gone.
func (b *Broker) pruneVerdicts(now time.Time) {
	for k, v := range b.verdicts {
		_, held := b.leases[v.winner]
		queued := slices.ContainsFunc(b.queue, func(l *lease) bool { return l.id == v.winner })
		if !now.Before(v.until) || (!held && !queued) {
			delete(b.verdicts, k)
		}
	}
}

// usage is card-seconds per engine key per UTC day, the default ranking.
type usage map[string]map[string]float64 // "2006-01-02" -> key -> seconds

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// accountUsage credits every lease on the card with the time since the last
// tick. A gap longer than a few polls (a stopped daemon, a suspended machine)
// is not credited.
func (b *Broker) accountUsage(now time.Time) {
	last := b.usageAt
	b.usageAt = now
	if last.IsZero() {
		return
	}
	dt := now.Sub(last)
	if dt <= 0 || dt > 10*b.cfg.Poll+10*time.Second {
		return
	}
	d := day(now)
	for _, l := range b.leases {
		if l.state != active && l.state != draining {
			continue
		}
		if b.usage[d] == nil {
			b.usage[d] = map[string]float64{}
		}
		b.usage[d][l.key()] += dt.Seconds()
		b.usageDirty = true
	}
}

func (b *Broker) usageHours(k string, now time.Time) float64 {
	var s float64
	for i := 0; i < usageDays; i++ {
		s += b.usage[day(now.AddDate(0, 0, -i))][k]
	}
	return float64(int(s/36)) / 100 // hours, two decimals, so a few seconds do not flip a rank
}

func loadUsage(path string) (usage, error) {
	u := usage{}
	data, err := os.ReadFile(path)
	if err != nil {
		return u, err
	}
	return u, json.Unmarshal(data, &u)
}

// persistUsage writes the usage file at most once a minute, and drops days
// older than the window.
func (b *Broker) persistUsage(now time.Time, force bool) {
	if b.cfg.UsageFile == "" {
		return
	}
	b.mu.Lock()
	if !b.usageDirty || (!force && now.Sub(b.usageSaved) < time.Minute) {
		b.mu.Unlock()
		return
	}
	cut := day(now.AddDate(0, 0, -usageDays))
	for d := range b.usage {
		if d < cut {
			delete(b.usage, d)
		}
	}
	data, err := json.Marshal(b.usage)
	b.usageDirty, b.usageSaved = false, now
	b.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(b.cfg.UsageFile, data)
	}
	if err != nil {
		b.log.Warn("engine usage not saved; the default ranking will not survive a restart", "file", b.cfg.UsageFile, "err", err)
	}
}

// priorityStatus is the ranking as the node applies it now.
func (b *Broker) priorityStatus(now time.Time) *proto.PriorityStatus {
	ps := &proto.PriorityStatus{Order: b.cfg.Order, JuggleWindow: b.juggleWindow().Seconds()}
	keys := map[string]bool{}
	for _, k := range b.cfg.Order {
		keys[k] = true
	}
	for i := 0; i < usageDays; i++ {
		for k := range b.usage[day(now.AddDate(0, 0, -i))] {
			keys[k] = true
		}
	}
	for _, l := range b.leases {
		keys[l.key()] = true
	}
	for k := range keys {
		ps.Engines = append(ps.Engines, proto.EngineRank{Engine: k, Hours7d: b.usageHours(k, now)})
	}
	sort.SliceStable(ps.Engines, func(i, j int) bool {
		a, c := ps.Engines[i], ps.Engines[j]
		if len(b.cfg.Order) > 0 {
			if ia, ic := orderIndex(b.cfg.Order, a.Engine), orderIndex(b.cfg.Order, c.Engine); ia != ic {
				return ia < ic
			}
		}
		if a.Hours7d != c.Hours7d {
			return a.Hours7d > c.Hours7d
		}
		return a.Engine < c.Engine
	})
	for i := range ps.Engines {
		ps.Engines[i].Rank = i + 1
	}
	if len(b.cfg.Order) > 0 {
		ps.Source = "order"
	} else {
		ps.Source = "usage"
	}
	return ps
}
