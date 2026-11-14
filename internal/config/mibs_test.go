// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// needs_mib on an entry is one size for every card, or a list with one size
// per card in gpu's order, for a model split across cards.
func TestEngineNeedsPerCard(t *testing.T) {
	const entry = "[[engine]]\nname = \"llama-70b\"\ntype = \"custom\"\nurl = \"http://127.0.0.1:8080\"\nhealth = \"/health\"\n"
	for _, tc := range []struct {
		body string
		want []uint64
	}{
		{"needs_mib = 8192\n", []uint64{8192 << 20}},
		{"gpu = \"gpu0,gpu1\"\nneeds_mib = 8192\n", []uint64{8192 << 20}},
		{"gpu = \"gpu0,gpu1\"\nneeds_mib = [18000, 6000]\n", []uint64{18000 << 20, 6000 << 20}},
		{"", nil},
		{"needs_mib = 0\n", nil},
	} {
		c, err := Load(write(t, entry+tc.body))
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if got := c.Engine[0].NeedsMiB.Bytes(); !slices.Equal(got, tc.want) {
			t.Errorf("%q: bytes = %v, want %v", tc.body, got, tc.want)
		}
	}
	for _, bad := range []struct{ body, wantErr string }{
		{"gpu = \"gpu0,gpu1\"\nneeds_mib = [18000, 6000, 2000]\n", `needs_mib 18000,6000,2000 has 3 sizes for 2 card(s) in gpu "gpu0,gpu1"`},
		{"needs_mib = [18000, 6000]\n", `needs_mib 18000,6000 has 2 sizes for 0 card(s) in gpu ""`},
		{"needs_mib = -1\n", "needs_mib is a number of MiB, or a list of them with one per card"},
		{"needs_mib = \"8G\"\n", "needs_mib is a number of MiB, or a list of them with one per card"},
	} {
		if _, err := Load(write(t, entry+bad.body)); err == nil || !strings.Contains(err.Error(), bad.wantErr) {
			t.Errorf("%q: error %v, want it to say %q", bad.body, err, bad.wantErr)
		}
	}
}

// learn_size learns one size on one card: it loads alone or with gpu naming
// one card, and refuses a typed size, several cards or whole_machine.
func TestEngineLearnSize(t *testing.T) {
	const entry = "[[engine]]\nname = \"llama\"\ntype = \"custom\"\nurl = \"http://127.0.0.1:8080\"\nhealth = \"/health\"\nlearn_size = true\n"
	for _, body := range []string{"", "gpu = \"gpu1\"\n"} {
		c, err := Load(write(t, entry+body))
		if err != nil || !c.Engine[0].LearnSize {
			t.Fatalf("%q: learn_size = %v, err %v; want true, nil", body, c.Engine[0].LearnSize, err)
		}
	}
	for _, bad := range []struct{ body, wantErr string }{
		{"needs_mib = 8192\n", "learn_size and needs_mib 8192 both size the lease"},
		{"gpu = \"gpu0,gpu1\"\n", `learn_size sizes a lease on one card, and gpu "gpu0,gpu1" names several`},
		{"whole_machine = true\n", "learn_size and whole_machine disagree"},
	} {
		if _, err := Load(write(t, entry+bad.body)); err == nil || !strings.Contains(err.Error(), bad.wantErr) {
			t.Errorf("%q: error %v, want it to say %q", bad.body, err, bad.wantErr)
		}
	}
}

// One size writes back as the plain number it always was, and a list as a
// list, in every encoding the config has.
func TestNeedsKeepTheirSpelling(t *testing.T) {
	for _, tc := range []struct {
		m                MiBs
		toml, json, yaml string
	}{
		{MiBs{8192}, "needs_mib = 8192", `"needs_mib":8192`, "needs_mib: 8192"},
		{MiBs{18000, 6000}, "needs_mib = [18000, 6000]", `"needs_mib":[18000,6000]`, "needs_mib:\n    - 18000\n    - 6000"},
	} {
		e := Engine{Name: "llama-70b", NeedsMiB: tc.m}
		var tb bytes.Buffer
		if err := toml.NewEncoder(&tb).Encode(e); err != nil {
			t.Fatal(err)
		}
		jb, _ := json.Marshal(e)
		yb, _ := yaml.Marshal(e)
		for _, got := range []struct{ enc, out, want string }{{"toml", tb.String(), tc.toml}, {"json", string(jb), tc.json}, {"yaml", string(yb), tc.yaml}} {
			if !strings.Contains(got.out, got.want) {
				t.Errorf("%s of %v = %s, want it to hold %s", got.enc, tc.m, got.out, got.want)
			}
		}
		var back Engine
		if err := json.Unmarshal(jb, &back); err != nil || !slices.Equal(back.NeedsMiB, tc.m) {
			t.Errorf("json back = %v (%v), want %v", back.NeedsMiB, err, tc.m)
		}
		if err := yaml.Unmarshal(yb, &back); err != nil || !slices.Equal(back.NeedsMiB, tc.m) {
			t.Errorf("yaml back = %v (%v), want %v", back.NeedsMiB, err, tc.m)
		}
	}
}

// berth run --needs-mib reads the same spellings.
func TestNeedsFlag(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    MiBs
		wantErr bool
	}{{"8192", MiBs{8192}, false}, {"18000,6000", MiBs{18000, 6000}, false}, {"18000, 6000", MiBs{18000, 6000}, false},
		{"8G", nil, true}, {"18000,", nil, true}} {
		var m MiBs
		err := m.Set(tc.in)
		if (err != nil) != tc.wantErr || !tc.wantErr && !slices.Equal(m, tc.want) {
			t.Errorf("Set(%q) = %v, %v; want %v, error %v", tc.in, m, err, tc.want, tc.wantErr)
		}
	}
}
