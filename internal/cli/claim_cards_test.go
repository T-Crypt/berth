// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/claims"
	"github.com/T-Crypt/berth/internal/node"
)

// A claim's gpu resolves to the node's card positions by name or PCI
// address; a card the node lacks is logged and the claim holds every card.
func TestClaimCardsResolvesTheRulesCards(t *testing.T) {
	n, err := node.New(nil, nil,
		&node.Card{Name: "gpu0", Addr: "0000:01:00.0"},
		&node.Card{Name: "gpu1", Addr: "0000:41:00.0"},
		&node.Card{Name: "gpu2", Addr: "0000:81:00.0"})
	if err != nil {
		t.Fatal(err)
	}
	rules := []claims.Rule{
		{Name: "all"},
		{Name: "named", GPU: "gpu2,00000000:41:00.0"},
		{Name: "missing", GPU: "gpu7"},
	}
	var logged bytes.Buffer
	claimCards(rules, n, slog.New(slog.NewTextHandler(&logged, nil)))
	for _, tc := range []struct {
		rule int
		want []int
	}{{0, nil}, {1, []int{2, 1}}, {2, nil}} {
		if got := rules[tc.rule].Cards; !slices.Equal(got, tc.want) {
			t.Errorf("%s: cards = %v, want %v", rules[tc.rule].Name, got, tc.want)
		}
	}
	if !strings.Contains(logged.String(), "claim=missing") || !strings.Contains(logged.String(), "holds every card") {
		t.Errorf("the missing card was not logged: %q", logged.String())
	}
}
