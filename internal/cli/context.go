package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
)

// View is one command's result in both encodings: the document a machine
// reads and the human report. ok is false when the command failed.
type View interface {
	Data() any
	Human(c *Context, ok bool) error
}

// Context is one command's run: the streams, the globals parsed once, and the
// format decided for the whole run.
type Context struct {
	Out     io.Writer // stdout, quiet-filtered once the flags are parsed
	Err     io.Writer // stderr: diagnostics, never suppressed by --quiet
	Args    []string  // the command's own args
	Cmd     *Command
	Version string
	Format  Format
	Verbose bool
	Quiet   bool
	Style   Style // for Out

	errStyle Style
	log      *slog.Logger
	setFlags map[string]bool
	// rawFormat is the --format given before the command name, where the
	// command's allowed values are not known yet; empty means none.
	rawFormat string
}

// FlagSet is the command's flag set: the globals first, then whatever the
// command registers on it. -h and --help are the flag package's own.
func (c *Context) FlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet(c.Cmd.Name, flag.ContinueOnError)
	// The help and the errors are the framework's to print, from an existing
	// Error: the flag package's own output would say everything twice.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	// One line per global: -v and -q are expanded into these names before the
	// parse, so registering both spellings would list each of them twice.
	fs.BoolVar(&c.Verbose, "verbose", c.Verbose, "more detail in the human report (short: -v)")
	fs.BoolVar(&c.Quiet, "quiet", c.Quiet, "no standard output; the exit code is the answer (short: -q)")
	if len(c.Cmd.Formats) > 0 {
		fs.Var(&formatValue{allowed: c.Cmd.Formats, got: &c.Format}, "format",
			"output format: "+formatsText(c.Cmd.Formats))
	}
	return fs
}

// expandShortForms lets -v and -q stand for their long names. Nothing after --
// is touched: that is the engine's own command line, and its flags are not
// ours to read.
func expandShortForms(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		switch a {
		case "-v":
			a = "-verbose"
		case "-q":
			a = "-quiet"
		}
		out = append(out, a)
	}
	return out
}

// Parse parses the command's own args and applies the globals. It returns
// (exit code, true) when the outcome is already handled: -h printed the help,
// or the flags were wrong. Anything left in fs.Args stays in c.Args.
func (c *Context) Parse(fs *flag.FlagSet) (int, bool) {
	err := fs.Parse(expandShortForms(c.Args))
	c.Args = fs.Args()
	if errors.Is(err, flag.ErrHelp) {
		c.help(fs)
		return ExitOK, true
	}
	if err != nil {
		c.fail(Systemf("berth %s: %v", c.Cmd.Name, err).
			WithHint("run \"berth %s -h\" for flags", c.Cmd.Name))
		return ExitSystem, true
	}
	c.setFlags = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { c.setFlags[f.Name] = true })
	if c.Quiet {
		c.Out = io.Discard
	}
	return ExitOK, false
}

// Changed says whether the flag was on the command line, so a command can tell
// "the default path" from "the path the operator named".
func (c *Context) Changed(name string) bool { return c.setFlags[name] }

// noArgs rejects a positional argument. Silently ignoring one would hide a
// mistyped flag or a path meant for --config.
func (c *Context) noArgs() (int, bool) {
	if len(c.Args) == 0 {
		return ExitOK, false
	}
	return c.failUnexpected(c.Args[0])
}

// failUnexpected reports one unexpected positional argument.
func (c *Context) failUnexpected(arg string) (int, bool) {
	c.fail(Systemf("unexpected argument %q", arg).
		WithHint("run \"berth %s -h\" for flags", c.Cmd.Name))
	return ExitSystem, true
}

// Printf writes to stdout, which --quiet empties.
func (c *Context) Printf(format string, a ...any) { fmt.Fprintf(c.Out, format, a...) }

// Bullet writes one "- " fact under a verdict line.
func (c *Context) Bullet(format string, a ...any) {
	fmt.Fprintf(c.Out, bulletIndent+format+"\n", a...)
}

// Sub writes one "* " fact one level deeper.
func (c *Context) Sub(format string, a ...any) {
	fmt.Fprintf(c.Out, subIndent+format+"\n", a...)
}

// Warnf is a diagnostic on stderr, so a warning never lands in a document.
func (c *Context) Warnf(format string, a ...any) {
	fmt.Fprintf(c.Err, "%s %s\n", c.errStyle.Warn(markWarn), fmt.Sprintf(format, a...))
}

// Errorf is a failure on stderr, outside the View a command returns.
func (c *Context) Errorf(format string, a ...any) {
	fmt.Fprintf(c.Err, "%s %s\n", c.errStyle.Err(markError), fmt.Sprintf(format, a...))
}

// Machine is true when the format is not the human one.
func (c *Context) Machine() bool { return c.Format != FormatHuman }

// Emit writes one document on stdout in the run's format.
func (c *Context) Emit(v any) error { return encode(c.Out, c.Format, v) }

// Stream is the encoder for a stream of values on stdout.
func (c *Context) Stream() *Stream { return &Stream{format: c.Format, w: c.Out} }

// Log is the command's logger: stderr, with the level -v and -q decided.
func (c *Context) Log() *slog.Logger {
	if c.log == nil {
		level := slog.LevelInfo
		if c.Verbose {
			level = slog.LevelDebug
		}
		if c.Quiet {
			level = slog.LevelError
		}
		c.log = slog.New(slog.NewTextHandler(c.Err, &slog.HandlerOptions{Level: level}))
	}
	return c.log
}

// Write emits a command's result. A nil err means the command passed. In a
// machine format the document is the whole report and the error stays off
// stderr: the exit code carries the verdict. In the human format the report
// goes to stdout and the failure to stderr. --quiet leaves stdout empty in
// both, and the exit code stays the answer.
func (c *Context) Write(v View, e *Error) int {
	if c.Machine() {
		if v != nil {
			if err := c.Emit(v.Data()); err != nil {
				e = Systemf("%v", err)
				c.fail(e)
				return e.Code
			}
		}
		// One report either way: the document when there is one, stderr when
		// the command had nothing to say (berthd unreachable, a bad path).
		if e != nil && v == nil {
			e.render(c.Err, c.errStyle)
		}
	} else {
		if v != nil {
			if err := v.Human(c, e == nil); err != nil {
				e = Systemf("%v", err)
			}
		}
		if e != nil {
			e.render(c.Err, c.errStyle)
		}
	}
	if e != nil {
		return e.Code
	}
	return ExitOK
}

// fail reports one error and returns its exit code.
func (c *Context) fail(e *Error) int {
	e.render(c.Err, c.errStyle)
	return e.Code
}

// help prints one command's usage line, its paragraph and its flags, to
// stdout: asking for help is not a failure.
func (c *Context) help(fs *flag.FlagSet) {
	args := ""
	if c.Cmd.Args != "" {
		args = " " + c.Cmd.Args
	}
	fmt.Fprintf(c.Out, "berth %s%s\n\n%s\n\nFlags:\n", c.Cmd.Name, args, c.Cmd.Help)
	fs.SetOutput(c.Out)
	fs.PrintDefaults()
}

// setFormat validates the --format value that came before the command name,
// where the command's own allowed values were not known yet.
func (c *Context) setFormat(v string) *Error {
	if v == "" {
		return nil
	}
	if len(c.Cmd.Formats) == 0 {
		return Systemf("--format does not apply to berth %s", c.Cmd.Name)
	}
	for _, f := range c.Cmd.Formats {
		if Format(v) == f {
			c.Format = f
			return nil
		}
	}
	return Systemf("invalid value %q for --format", v).
		Detail("valid values: %s", formatsText(c.Cmd.Formats))
}
