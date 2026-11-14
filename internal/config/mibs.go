// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MiBs is an engine's card memory in MiB, one value per card it runs on, in
// the order its gpu key names them: needs_mib = [18000, 6000] for an engine
// split unevenly across gpu0 and gpu1. One value is the size on every card,
// and none (or 0) is the whole card. It reads as a number or a list in TOML,
// JSON and YAML, and as "18000" or "18000,6000" on the command line; one
// value writes back as a plain number.
type MiBs []uint64

var errMiBs = errors.New("needs_mib is a number of MiB, or a list of them with one per card")

// Bytes is the size on each card in bytes, nil for the whole card.
func (m MiBs) Bytes() []uint64 {
	if m.whole() {
		return nil
	}
	out := make([]uint64, len(m))
	for i, v := range m {
		out[i] = v << 20
	}
	return out
}

// whole is true when no card is sized: no value, or the single value 0.
func (m MiBs) whole() bool { return len(m) == 0 || len(m) == 1 && m[0] == 0 }

// String is the command-line spelling: "18000" or "18000,6000".
func (m MiBs) String() string {
	parts := make([]string, len(m))
	for i, v := range m {
		parts[i] = strconv.FormatUint(v, 10)
	}
	return strings.Join(parts, ",")
}

// Set parses the command-line spelling, for flag.Var.
func (m *MiBs) Set(s string) error {
	var out MiBs
	for _, p := range strings.Split(s, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return fmt.Errorf("%q: %w", s, errMiBs)
		}
		out = append(out, v)
	}
	*m = out
	return nil
}

// UnmarshalTOML reads a TOML integer or array of integers.
func (m *MiBs) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		return m.set([]int64{v})
	case []any:
		ns := make([]int64, len(v))
		for i, e := range v {
			n, ok := e.(int64)
			if !ok {
				return errMiBs
			}
			ns[i] = n
		}
		return m.set(ns)
	}
	return errMiBs
}

func (m *MiBs) set(ns []int64) error {
	if len(ns) == 0 {
		return errMiBs
	}
	out := make(MiBs, len(ns))
	for i, n := range ns {
		if n < 0 {
			return errMiBs
		}
		out[i] = uint64(n)
	}
	*m = out
	return nil
}

func (m MiBs) MarshalTOML() ([]byte, error) {
	if len(m) == 1 {
		return []byte(strconv.FormatUint(m[0], 10)), nil
	}
	return []byte("[" + strings.ReplaceAll(m.String(), ",", ", ") + "]"), nil
}

func (m MiBs) MarshalJSON() ([]byte, error) {
	if len(m) == 1 {
		return json.Marshal(m[0])
	}
	return json.Marshal([]uint64(m))
}

func (m *MiBs) UnmarshalJSON(b []byte) error {
	var one uint64
	if json.Unmarshal(b, &one) == nil {
		*m = MiBs{one}
		return nil
	}
	var each []uint64
	if err := json.Unmarshal(b, &each); err != nil || len(each) == 0 {
		return errMiBs
	}
	*m = each
	return nil
}

func (m MiBs) MarshalYAML() (any, error) {
	if len(m) == 1 {
		return m[0], nil
	}
	return []uint64(m), nil
}

func (m *MiBs) UnmarshalYAML(node *yaml.Node) error {
	var one uint64
	if node.Kind == yaml.ScalarNode && node.Decode(&one) == nil {
		*m = MiBs{one}
		return nil
	}
	var each []uint64
	if node.Kind != yaml.SequenceNode || node.Decode(&each) != nil || len(each) == 0 {
		return errMiBs
	}
	*m = each
	return nil
}

// CheckLearnSize is the rule for learn_size and berth run --learn-size: it
// learns one size on one card, so it takes no typed size, no list of cards
// and no whole_machine.
func CheckLearnSize(e Engine) error {
	switch {
	case !e.LearnSize:
		return nil
	case len(e.NeedsMiB.Bytes()) > 0:
		return fmt.Errorf("learn_size and needs_mib %s both size the lease: keep one", e.NeedsMiB)
	case strings.Contains(e.GPU, ","):
		return fmt.Errorf("learn_size sizes a lease on one card, and gpu %q names several", e.GPU)
	case e.WholeMachine:
		return errors.New("learn_size and whole_machine disagree: whole_machine takes every card")
	}
	return nil
}

// CheckNeeds is the rule for needs_mib and berth run --needs-mib: a list of
// sizes has one size per card that gpu names.
func CheckNeeds(m MiBs, gpu string) error {
	if len(m) < 2 {
		return nil
	}
	cards := 0
	if gpu != "" {
		cards = len(strings.Split(gpu, ","))
	}
	if cards != len(m) {
		return fmt.Errorf("needs_mib %s has %d sizes for %d card(s) in gpu %q: give one size, or one per card in the same order",
			m, len(m), cards, gpu)
	}
	return nil
}
