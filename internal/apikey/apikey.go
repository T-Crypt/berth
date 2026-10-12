// Package apikey is the node's API key: one random secret per install, in a
// file only its owner can read, generated the first time something needs it.
// The gateway takes it as a bearer token; clients on the node read it with
// `berth key`, so it never has to be pasted into a config file.
package apikey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath is $BERTH_API_KEY_FILE, else api.key beside berth.toml.
func DefaultPath() string {
	if p := os.Getenv("BERTH_API_KEY_FILE"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "berth-api.key"
	}
	return filepath.Join(dir, "berth", "api.key")
}

// LoadOrCreate reads the key at path, creating it (0600, in a 0700 directory
// when the directory is new) when there is none. A key file others can read
// is refused rather than used: it would be a secret in name only.
func LoadOrCreate(path string) (string, error) {
	key, err := load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key = "berth-" + hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return load(path) // another process made it first
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return key, f.Close()
}

func load(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %04o); chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if len(key) < 16 {
		return "", fmt.Errorf("%s holds no usable key (under 16 characters)", path)
	}
	return key, nil
}

// Bearer reports whether the Authorization header carries key.
func Bearer(header, key string) bool {
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(key)) == 1
}
