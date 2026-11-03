// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// APIKey resolves exactly one credential source without storing its value in config.
func (e Engine) APIKey() (string, error) {
	if e.APIKeyEnv != "" && e.APIKeyFile != "" {
		return "", fmt.Errorf("api_key_env and api_key_file are mutually exclusive")
	}
	if e.APIKeyEnv != "" {
		key := os.Getenv(e.APIKeyEnv)
		if len(strings.Fields(key)) != 1 || strings.TrimSpace(key) != key {
			return "", fmt.Errorf("credential environment variable %s must contain one nonempty token", e.APIKeyEnv)
		}
		return key, nil
	}
	if e.APIKeyFile == "" {
		return "", nil
	}
	return credentialFile(e.APIKeyFile)
}

// credentialFile reads the one token in an absolute, owner-private file.
func credentialFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("credential file must use an absolute path")
	}
	f, err := openCredential(path)
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("credential file metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("credential file must be regular and private to its owner")
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("credential file read: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if len(strings.Fields(key)) != 1 {
		return "", fmt.Errorf("credential file must contain one nonempty token")
	}
	return key, nil
}
