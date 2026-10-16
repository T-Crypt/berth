package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultSocket is $BERTH_SOCKET, else $XDG_RUNTIME_DIR/berth.sock, else a
// per-user file in the temp dir.
func defaultSocket() string {
	if s := os.Getenv("BERTH_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "berth.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("berth-%d.sock", os.Getuid()))
}

// defaultStateFile is $XDG_STATE_HOME/berth/NAME, falling back to
// ~/.local/state/berth/NAME: kept across reboots, unlike the runtime dir.
func defaultStateFile(name string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "berth", name)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "berth", name)
	}
	return filepath.Join(os.TempDir(), "berth", name)
}

// defaultHarborDB is the SQLite file `berth harbor` persists its history in.
func defaultHarborDB() string { return defaultStateFile("harbor.db") }
