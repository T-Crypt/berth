package cli

import (
	"strings"
	"testing"
)

// The command list, the dispatch and the help all read one table: a row that
// cannot be dispatched or cannot describe itself is a bug this catches early.
func TestRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, cmd := range commands {
		if cmd.Name == "" || cmd.Summary == "" || cmd.Help == "" {
			t.Errorf("command %q: name, summary and help are all required (%+v)", cmd.Name, cmd)
		}
		if seen[cmd.Name] {
			t.Errorf("command %q is listed twice", cmd.Name)
		}
		seen[cmd.Name] = true
		switch {
		case len(cmd.Sub) > 0 && cmd.Run != nil:
			t.Errorf("command %q is both a group and a leaf", cmd.Name)
		case len(cmd.Sub) == 0 && cmd.Run == nil:
			t.Errorf("command %q has neither Sub nor Run", cmd.Name)
		}
		if len(cmd.Sub) > 0 && cmd.Formats != nil {
			t.Errorf("group %q declares formats; a group has no output", cmd.Name)
		}
		if len(cmd.Formats) > 0 && cmd.Formats[0] != FormatHuman {
			t.Errorf("command %q: human must be the first format, got %v", cmd.Name, cmd.Formats)
		}
		subs := map[string]bool{}
		for _, sub := range cmd.Sub {
			if sub.Name == "" || sub.Summary == "" || sub.Help == "" {
				t.Errorf("%s %q: name, summary and help are all required", cmd.Name, sub.Name)
			}
			if subs[sub.Name] {
				t.Errorf("%s %q is listed twice", cmd.Name, sub.Name)
			}
			subs[sub.Name] = true
			if sub.Run == nil {
				t.Errorf("%s %q has no Run", cmd.Name, sub.Name)
			}
		}
	}
}

func TestOverviewListsEveryCommand(t *testing.T) {
	isolate(t)
	_, out, _ := runCLI(t, "help")
	for _, cmd := range commands {
		if !strings.Contains(out, cmd.Name) {
			t.Errorf("the command list lacks %q", cmd.Name)
		}
		for _, sub := range cmd.Sub {
			if !strings.Contains(out, sub.Name) {
				t.Errorf("the command list lacks %q", cmd.Name+" "+sub.Name)
			}
		}
	}
}
