package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/T-Crypt/berth/internal/export"
	"github.com/T-Crypt/berth/internal/store"
)

// exportDoc is what `berth export` leaves on stdout: where the dump went and
// what is in it, so a script can pick the files up.
type exportDoc struct {
	Status string    `json:"status" yaml:"status"`
	Range  string    `json:"range" yaml:"range"`
	Out    string    `json:"out" yaml:"out"`
	Files  []fileDoc `json:"files" yaml:"files"`
}

type fileDoc struct {
	Kind string `json:"kind" yaml:"kind"` // requests | samples
	Path string `json:"path" yaml:"path"`
	Rows int    `json:"rows" yaml:"rows"`
}

type exportView struct{ doc exportDoc }

func (v exportView) Data() any { return v.doc }

func (v exportView) Human(c *Context, ok bool) error {
	c.Printf("%s Exported %s to %s\n", c.Style.OK(markOK), v.doc.Range, v.doc.Out)
	for _, f := range v.doc.Files {
		path := filepath.Base(f.Path)
		if c.Verbose {
			path = f.Path
		}
		c.Bullet("%s: %d row%s", path, f.Rows, plural(f.Rows))
	}
	return nil
}

// runExport dumps the harbor's stored history to the operator's own analysis:
// the requests table and the history samples of one range, CSV or JSON. It
// reads through the store's read methods only and never creates a store file.
func runExport(c *Context) int {
	fs := c.FlagSet()
	db := fs.String("db", defaultHarborDB(), "the harbor's SQLite file (what berth harbor persists to)")
	rng := fs.String("range", "30d", "how far back: 1h, 6h, 24h, 7d or 30d")
	as := fs.String("as", "csv", "the file format: csv or json")
	out := fs.String("out", ".", "directory the dump is written to")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	d, ok := store.ParseRange(*rng)
	if !ok {
		return c.fail(Systemf("range %q is not one of 1h, 6h, 24h, 7d, 30d", *rng))
	}
	if *as != "csv" && *as != "json" {
		return c.fail(Systemf("--as %q is not csv or json", *as))
	}
	if _, err := os.Stat(*db); err != nil {
		return c.fail(Systemf("no harbor store at %s", *db).Detail("run berth harbor first"))
	}
	// retention 0 takes the default; irrelevant, the command never prunes.
	st, err := store.Open(*db, 0)
	if err != nil {
		return c.fail(Systemf("%v", err))
	}
	defer st.Close()
	now := time.Now()
	reqs, err := st.Requests(d, now)
	if err != nil {
		return c.fail(Systemf("%v", err))
	}
	hh, err := st.History(d, now)
	if err != nil {
		return c.fail(Systemf("%v", err))
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return c.fail(Systemf("%v", err))
	}
	ext := *as
	write := func(file string, csvW func(io.Writer) error, jsonW func(*json.Encoder) error) error {
		f, err := os.Create(file)
		if err != nil {
			return err
		}
		var w error
		if *as == "csv" {
			w = csvW(f)
		} else {
			w = jsonW(json.NewEncoder(f))
		}
		if cerr := f.Close(); w == nil {
			w = cerr
		}
		return w
	}
	requests := filepath.Join(*out, "requests."+ext)
	if err := write(requests,
		func(w io.Writer) error { return export.RequestsCSV(reqs, w) },
		func(e *json.Encoder) error { return e.Encode(reqs) }); err != nil {
		return c.fail(Systemf("%v", err))
	}
	samples := filepath.Join(*out, "samples."+ext)
	if err := write(samples,
		func(w io.Writer) error { return export.SamplesCSV(hh, w) },
		func(e *json.Encoder) error { return e.Encode(hh) }); err != nil {
		return c.fail(Systemf("%v", err))
	}
	return c.Write(exportView{doc: exportDoc{
		Status: "ok", Range: *rng, Out: *out,
		Files: []fileDoc{
			{Kind: "requests", Path: requests, Rows: len(reqs)},
			{Kind: "samples", Path: samples, Rows: len(hh.Samples)},
		},
	}}, nil)
}
