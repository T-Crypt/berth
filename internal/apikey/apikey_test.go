package apikey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth", "api.key")
	k1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k1, "berth-") || len(k1) != len("berth-")+64 {
		t.Fatalf("key %q", k1)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, %v", st.Mode().Perm(), err)
	}
	k2, err := LoadOrCreate(path)
	if err != nil || k2 != k1 {
		t.Fatalf("second load %q, %v; want the same key", k2, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a world-readable key was used: %v", err)
	}
}

func TestBearer(t *testing.T) {
	const key = "berth-0123456789abcdef"
	for h, want := range map[string]bool{
		"Bearer " + key:       true,
		"bearer " + key:       true,
		"Bearer " + key + "x": false,
		"Basic " + key:        false,
		key:                   false,
		"":                    false,
	} {
		if Bearer(h, key) != want {
			t.Errorf("Bearer(%q) = %v, want %v", h, !want, want)
		}
	}
	if Bearer("Bearer ", "") {
		t.Error("an empty key matched")
	}
}
