// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate points every path the CLI reads at a temp dir, so a test never
// touches the operator's config, socket or key.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("BERTH_SOCKET", filepath.Join(home, "run", "berth.sock"))
	t.Setenv("BERTH_CONFIG", "")
	t.Setenv("BERTH_API_KEY_FILE", filepath.Join(home, "config", "berth", "api.key"))
	t.Setenv("NO_COLOR", "1")
	return home
}

// runCLI runs one CLI invocation in process, with output captured.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Env{Args: args, Out: &out, Err: &errOut, Version: "test"})
	return code, out.String(), errOut.String()
}

// tomlPath makes p a TOML basic string's body: a Windows root carries
// backslashes, which are escape sequences in TOML.
func tomlPath(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}

func writeGGUF(t *testing.T, path, arch, name string, ctx uint32, ftype uint32, split ...uint32) string {
	t.Helper()

	var b bytes.Buffer
	h := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) {
		h(uint64(len(s)))
		b.WriteString(s)
	}
	b.WriteString("GGUF")
	h(uint32(3)) // version
	h(uint64(1)) // one tensor
	pairs := [][2]string{{"general.architecture", arch}, {"general.name", name}}
	n := len(pairs) + 1 + boolInt(ctx > 0) + boolInt(len(split) > 0 && split[0] > 0)
	h(uint64(n))
	for _, kv := range pairs {
		str(kv[0])
		h(uint32(8)) // string
		str(kv[1])
	}
	str("general.file_type")
	h(uint32(4)) // uint32
	h(ftype)
	if len(split) > 0 && split[0] > 0 {
		str("split.count")
		h(uint32(4))
		h(split[0])
	}
	if ctx > 0 {
		str(arch + ".context_length")
		h(uint32(4))
		h(ctx)
	}
	// One tensor: name, rank 2, its two dimensions, its type and an offset.
	str("blk.0.weight")
	h(uint32(2))
	h(uint64(8))
	h(uint64(8))
	h(uint32(1)) // F16
	h(uint64(0))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func library(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeGGUF(t, filepath.Join(dir, "Qwen3-8B-Q4_K_M.gguf"), "qwen2", "Qwen3 8B", 32768, 15)
	writeGGUF(t, filepath.Join(dir, "Qwen3-8B-Q8_0.gguf"), "qwen2", "Qwen3 8B", 32768, 7)
	writeGGUF(t, filepath.Join(dir, "Qwen3-8B-mmproj-f16.gguf"), "clip", "Qwen3 8B vision", 0, 1)
	writeGGUF(t, filepath.Join(dir, "embed", "nomic-embed.gguf"), "bert", "Nomic Embed", 2048, 15)
	writeGGUF(t, filepath.Join(dir, "deep", "part-00001-of-00002.gguf"), "llama", "Deep Split", 4096, 15, 2)
	writeGGUF(t, filepath.Join(dir, "deep", "part-00002-of-00002.gguf"), "llama", "Deep Split", 4096, 15, 2)
	return dir
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// waitUntil polls until f is true, so async daemon-side releases settle.
func waitUntil(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never became true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
