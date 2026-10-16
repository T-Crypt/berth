package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// Format is one output encoding. Every command that prints a result accepts
// human (the default), json and yaml; `berth config show` also accepts toml,
// the config's own file format.
type Format string

const (
	FormatHuman Format = "human"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatTOML  Format = "toml"
)

// formatsText names the values of a list, in order, for a flag error and the
// --format help line.
func formatsText(fs []Format) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// formatValue is the --format flag: it accepts only the values the command
// declared, so the error names them.
type formatValue struct {
	allowed []Format
	got     *Format
}

func (v *formatValue) String() string {
	if v.got == nil {
		return ""
	}
	return string(*v.got)
}

func (v *formatValue) Set(s string) error {
	for _, f := range v.allowed {
		if Format(s) == f {
			*v.got = f
			return nil
		}
	}
	return fmt.Errorf("valid values: %s", formatsText(v.allowed))
}

// Markers and indentation. The human layout puts the verdict on the first line
// and its facts as bullets under it, so the tag column reads as a list.
const (
	markOK    = "[OK]"
	markWarn  = "[WARN]"
	markError = "[ERROR]"

	bulletIndent = "     - "    // under "[OK] "
	subIndent    = "       * "  // one level deeper
	errIndent    = "        - " // under "[ERROR] "
	errText      = "          " // a hint, aligned with the text of a detail
)

// ANSI codes. Colour is sparing by design: a tag, a state word, a timestamp.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// Style paints a string with ANSI colour when its stream is a terminal and
// NO_COLOR is unset. Every method returns its input unchanged when colour is
// off, so a piped or logged run stays plain.
type Style struct{ on bool }

// newStyle decides once per stream: a terminal that is not dumb and has not
// asked for no colour.
func newStyle(w io.Writer) Style {
	if os.Getenv("NO_COLOR") != "" {
		return Style{}
	}
	if term := os.Getenv("TERM"); term == "" || term == "dumb" {
		return Style{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return Style{}
	}
	fi, err := f.Stat()
	return Style{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (s Style) paint(code, v string) string {
	if !s.on {
		return v
	}
	return code + v + ansiReset
}

func (s Style) OK(v string) string   { return s.paint(ansiGreen, v) }
func (s Style) Warn(v string) string { return s.paint(ansiYellow, v) }
func (s Style) Err(v string) string  { return s.paint(ansiRed, v) }
func (s Style) Bold(v string) string { return s.paint(ansiBold, v) }
func (s Style) Dim(v string) string  { return s.paint(ansiDim, v) }

// encode writes one value in the format's encoding. JSON is indented because a
// person reads these documents too; the stream form below stays compact.
func encode(w io.Writer, f Format, v any) error {
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	case FormatYAML:
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		defer enc.Close()
		return enc.Encode(v)
	case FormatTOML:
		return toml.NewEncoder(w).Encode(v)
	}
	return fmt.Errorf("no encoding for format %q", f)
}

// Stream writes values of one stream: a compact JSON object per line, or one
// YAML document per value. Both encoders have to stay open for the whole
// stream (a YAML encoder separates the documents with ---), so a caller that
// follows events holds the Stream, not a writer.
type Stream struct {
	format Format
	w      io.Writer
	json   *json.Encoder
	yaml   *yaml.Encoder
}

func (s *Stream) Write(v any) error {
	switch s.format {
	case FormatJSON:
		if s.json == nil {
			s.json = json.NewEncoder(s.w)
			s.json.SetEscapeHTML(false)
		}
		return s.json.Encode(v)
	case FormatYAML:
		if s.yaml == nil {
			s.yaml = yaml.NewEncoder(s.w)
			s.yaml.SetIndent(2)
		}
		return s.yaml.Encode(v)
	}
	return fmt.Errorf("no stream encoding for format %q", s.format)
}
