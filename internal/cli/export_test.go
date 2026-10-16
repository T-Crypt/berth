package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/store"
)

// harborStore makes an empty store file, which is what a harbor that has run
// once leaves behind.
func harborStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harbor.db")
	st, err := store.Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExportWritesBothFiles(t *testing.T) {
	isolate(t)
	db := harborStore(t)
	out := filepath.Join(t.TempDir(), "dump")
	code, stdout, errOut := runCLI(t, "export", "--db", db, "--range", "1h", "--out", out)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(stdout, "[OK] Exported 1h to "+out) {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "requests.csv: 0 rows") || !strings.Contains(stdout, "samples.csv") {
		t.Errorf("stdout %q does not name both files", stdout)
	}
	for _, name := range []string{"requests.csv", "samples.csv"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExportJSONNamesTheFilesAndRows(t *testing.T) {
	isolate(t)
	db := harborStore(t)
	out := filepath.Join(t.TempDir(), "dump")
	code, stdout, _ := runCLI(t, "export", "--db", db, "--range", "24h", "--out", out, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var doc exportDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json %q: %v", stdout, err)
	}
	if doc.Status != "ok" || doc.Range != "24h" || doc.Out != out {
		t.Errorf("doc = %+v", doc)
	}
	if len(doc.Files) != 2 || doc.Files[0].Kind != "requests" || doc.Files[1].Kind != "samples" {
		t.Errorf("files = %+v", doc.Files)
	}
	if doc.Files[0].Path != filepath.Join(out, "requests.csv") {
		t.Errorf("path = %q", doc.Files[0].Path)
	}
}

func TestExportAsJSONChangesTheFileFormat(t *testing.T) {
	isolate(t)
	db := harborStore(t)
	out := filepath.Join(t.TempDir(), "dump")
	if code, _, errOut := runCLI(t, "export", "--db", db, "--range", "1h", "--out", out, "--as", "json"); code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, name := range []string{"requests.json", "samples.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExportErrors(t *testing.T) {
	isolate(t)
	db := harborStore(t)
	out := filepath.Join(t.TempDir(), "dump")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a missing store", []string{"export", "--db", filepath.Join(t.TempDir(), "nope.db")}, "no harbor store at"},
		{"a range that does not exist", []string{"export", "--db", db, "--range", "3h", "--out", out}, `range "3h"`},
		{"a file format that does not exist", []string{"export", "--db", db, "--as", "tsv", "--out", out}, `--as "tsv"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, errOut := runCLI(t, tc.args...)
			if code != ExitSystem {
				t.Fatalf("exit = %d, want %d", code, ExitSystem)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want %q", errOut, tc.want)
			}
		})
	}
}
