// Package cli is the berth command tree: one flag set, one output contract and
// one exit-code contract for every command. A command returns a View, which is
// the document a machine reads in a machine format and the report a person
// reads in the human one, plus an Error when it failed.
package cli

import (
	"fmt"
	"io"
	"strings"
)

// Exit codes, per the CLI contract. berth run passes its own codes through:
// the engine's exit status, or runner.ExitRefused (75) when no berth was
// granted.
const (
	ExitOK     = 0
	ExitFail   = 1 // validation or logic: the request was refused by what it checked
	ExitSystem = 2 // could not do the job: bad flags, unreadable file, berthd unreachable
)

// Command is one row of the command tree: a leaf, or a group with Sub.
type Command struct {
	Name    string   // "status", or "config" for a group
	Summary string   // one line for the command list
	Args    string   // usage tail, e.g. "[-f] [--since N]"; empty when it takes none
	Help    string   // the paragraph `berth help status` prints
	Formats []Format // --format values it accepts; nil means no --format flag
	Sub     []*Command
	Run     func(*Context) int
}

// Env is one invocation of the binary.
type Env struct {
	Args    []string // argv without the program name
	Out     io.Writer
	Err     io.Writer
	Version string
}

// Run parses argv, runs one command and returns the process exit code.
func Run(env Env) int {
	c := &Context{
		Out: env.Out, Err: env.Err, Version: env.Version,
		Format: FormatHuman, Style: newStyle(env.Out), errStyle: newStyle(env.Err),
	}
	args, code, done := c.parseGlobals(env.Args)
	if done {
		return code
	}
	if len(args) == 0 {
		c.overview(c.Err)
		return ExitSystem
	}
	if args[0] == "help" {
		return c.helpCommand(args[1:])
	}
	cmd := lookup(commands, args[0])
	if cmd == nil {
		return c.fail(Systemf("unknown command %q", args[0]).
			WithHint("run \"berth help\" for the command list"))
	}
	rest := args[1:]
	if len(cmd.Sub) > 0 {
		if len(rest) == 0 {
			c.groupHelp(cmd, c.Err)
			return ExitSystem
		}
		switch rest[0] {
		case "-h", "-help", "--help":
			c.groupHelp(cmd, c.Out)
			return ExitOK
		}
		sub := lookup(cmd.Sub, rest[0])
		if sub == nil {
			return c.fail(Systemf("unknown command %q", cmd.Name+" "+rest[0]).
				WithHint("run \"berth help %s\" for the list", cmd.Name))
		}
		cmd, rest = sub, rest[1:]
	}
	c.Cmd = cmd
	c.Args = rest
	if e := c.setFormat(c.rawFormat); e != nil {
		return c.fail(e)
	}
	return cmd.Run(c)
}

// parseGlobals reads the global flags that come before the command name, so
// `berth -q status` and `berth status -q` mean the same thing. It stops at the
// first token that is not one of them: that token is the command.
func (c *Context) parseGlobals(args []string) (rest []string, code int, done bool) {
	for len(args) > 0 {
		name, value, hasValue := strings.Cut(args[0], "=")
		switch name {
		case "-v", "-verbose", "--verbose":
			c.Verbose = true
		case "-q", "-quiet", "--quiet":
			c.Quiet = true
		case "-h", "-help", "--help":
			c.overview(c.Out)
			return nil, ExitOK, true
		case "-version", "--version":
			fmt.Fprintf(c.Out, "berth %s\n", c.Version)
			return nil, ExitOK, true
		case "-format", "--format":
			if !hasValue {
				if len(args) < 2 {
					return nil, c.fail(Systemf("--format needs a value")), true
				}
				value, args = args[1], args[1:]
			}
			c.rawFormat = value
		default:
			if strings.HasPrefix(args[0], "-") && args[0] != "-" {
				return nil, c.fail(Systemf("unknown flag %q", args[0]).
					WithHint("run \"berth help\" for the command list")), true
			}
			return args, ExitOK, false
		}
		args = args[1:]
	}
	return args, ExitOK, false
}

// helpCommand is `berth help [command] [sub]`. A leaf prints its own help, the
// same text `-h` prints, so the flags have one source.
func (c *Context) helpCommand(args []string) int {
	if len(args) == 0 {
		c.overview(c.Out)
		return ExitOK
	}
	cmd := lookup(commands, args[0])
	if cmd == nil {
		return c.fail(Systemf("unknown command %q", args[0]).
			WithHint("run \"berth help\" for the command list"))
	}
	rest := args[1:]
	if len(cmd.Sub) > 0 {
		if len(rest) == 0 {
			c.groupHelp(cmd, c.Out)
			return ExitOK
		}
		sub := lookup(cmd.Sub, rest[0])
		if sub == nil {
			return c.fail(Systemf("unknown command %q", args[0]+" "+rest[0]).
				WithHint("run \"berth help %s\" for the list", args[0]))
		}
		cmd, rest = sub, rest[1:]
	}
	if len(rest) > 0 {
		return c.fail(Systemf("berth %s takes no further argument %q", cmd.Name, rest[0]))
	}
	c.Cmd = cmd
	c.Args = []string{"-h"}
	return cmd.Run(c)
}

// lookup finds a row by name.
func lookup(list []*Command, name string) *Command {
	for _, cmd := range list {
		if cmd.Name == name {
			return cmd
		}
	}
	return nil
}

// overview is the command list: what `berth` (to stderr, a usage error),
// `berth -h` and `berth help` (to stdout) print.
func (c *Context) overview(w io.Writer) {
	fmt.Fprintf(w, "berth %s - one GPU, many inference engines, taking turns\n\n", c.Version)
	fmt.Fprint(w, "  berth <command> [flags]\n\n")
	for _, cmd := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", cmd.Name, cmd.Summary)
		for _, sub := range cmd.Sub {
			fmt.Fprintf(w, "    %-7s %s\n", sub.Name, sub.Summary)
		}
	}
	fmt.Fprint(w, `
  Global flags:
    -v, --verbose   more detail in the human report
    -q, --quiet     no standard output; the exit code is the answer
    --format F      human (default), json or yaml, where the command takes it

  Results go to stdout, diagnostics to stderr.
  Run "berth help <command>" for one command's flags.
`)
}

// groupHelp lists a group's subcommands.
func (c *Context) groupHelp(cmd *Command, w io.Writer) {
	fmt.Fprintf(w, "berth %s <command> [flags]\n\n%s\n\n", cmd.Name, cmd.Help)
	for _, sub := range cmd.Sub {
		fmt.Fprintf(w, "  %-7s %s\n", sub.Name, sub.Summary)
	}
	fmt.Fprintf(w, "\n  Run \"berth help %s <command>\" for one command's flags.\n", cmd.Name)
}

// plural is the s a count needs.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
