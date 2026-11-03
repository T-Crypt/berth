// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// Peer is another node's gateway. A model no engine here serves is sent
// to the first peer that lists it, and that node's own berthd leases its
// own card: a lease never crosses a machine.
type Peer struct {
	Name string `toml:"name" json:"name" yaml:"name"`
	// URL is the peer's gateway as this machine reaches it over the
	// tailnet: its tailscale serve name, or a tailnet address.
	URL string `toml:"url" json:"url" yaml:"url"`
	// KeyFile holds the peer's node key (berth key on that machine): an
	// absolute, owner-private file.
	KeyFile string `toml:"key_file" json:"key_file" yaml:"key_file"`
}

// Key reads the peer's node key.
func (p Peer) Key() (string, error) { return credentialFile(p.KeyFile) }

// tailnetV4 is the range Tailscale gives its nodes (100.64.0.0/10), and
// tailnetV6 its IPv6 prefix.
var (
	_, tailnetV4, _ = net.ParseCIDR("100.64.0.0/10")
	_, tailnetV6, _ = net.ParseCIDR("fd7a:115c:a1e0::/48")
)

// validPeer checks one [[peer]] entry. The peer's node key travels with
// every request to it, so the URL must stay on this machine or the
// tailnet: loopback, a *.ts.net name, or a tailnet address.
func validPeer(p Peer) error {
	if p.Name == "" {
		return fmt.Errorf("needs a name")
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("url %q is not an http or https URL", p.URL)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("url %q names a path; give the gateway's base URL", p.URL)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	switch {
	case host == "localhost", strings.HasSuffix(host, ".ts.net"):
	case ip != nil && (ip.IsLoopback() || tailnetV4.Contains(ip) || tailnetV6.Contains(ip)):
	default:
		return fmt.Errorf("url %q is not on the tailnet: use the peer's *.ts.net name or its 100.x address", p.URL)
	}
	if !filepath.IsAbs(p.KeyFile) {
		return fmt.Errorf("key_file must be an absolute path to the peer's node key")
	}
	return nil
}
