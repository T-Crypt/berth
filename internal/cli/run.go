package cli

import (
	"fmt"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/runner"
)

func runRun(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	engine := fs.String("as", "", "engine name shown in status and drain messages (required)")
	group := fs.String("group", "", "engine family for juggles and the usage ranking, e.g. llama-swap for each of its models (default: --as)")
	prio := fs.String("priority", "interactive", "batch, interactive or foreground")
	needs := fs.Uint64("needs-mib", 0, "memory this engine needs; 0 = the whole card")
	wait := fs.Duration("wait", 0, "how long to wait for a berth; 0 = berthd's default")
	drainPost := fs.String("drain-post", "", "URL to POST when asked to drain, before signalling (e.g. llama-swap's unload)")
	drainComfy := fs.String("drain-comfyui", "", "ComfyUI base URL: on drain, wait until its queue is empty before stopping it (never cuts a job off)")
	keepComfy := fs.Bool("keep-comfyui-up", false, "with --drain-comfyui: unload its models via POST /free and leave the server up instead of stopping it")
	grace := fs.Duration("grace", 30*time.Second, "time each drain step gets before the next")
	failClosed := fs.Bool("fail-closed", false, "do not start CMD when berthd is unreachable")
	if code, done := c.Parse(fs); done {
		return code
	}
	if *engine == "" || len(c.Args) == 0 {
		return c.fail(Systemf("--as NAME and the engine's command are required").
			WithHint("usage: berth run --as NAME [flags] -- CMD [ARGS...]"))
	}
	if _, err := broker.ParsePriority(*prio); err != nil {
		return c.fail(Systemf("%v", err))
	}
	if err := checkRunFlags(*keepComfy, *drainComfy); err != nil {
		return c.fail(Systemf("%v", err))
	}
	// Nothing here goes to stdout: the engine owns it. berth's own notices are
	// diagnostics on stderr, and --quiet never hides the unprotected warning.
	return runner.Run(runner.Options{
		Socket: *sock, Engine: *engine, Group: *group, Priority: *prio, Needs: *needs * gpu.MiB, Wait: *wait,
		DrainPost: *drainPost, DrainComfyUI: *drainComfy, KeepComfyUI: *keepComfy, Grace: *grace,
		FailClosed: *failClosed, Argv: c.Args, Stderr: c.Err,
	})
}

// checkRunFlags rejects flag combinations that can never work, mirroring
// the berth.toml validation, before anything starts.
func checkRunFlags(keepComfy bool, drainComfy string) error {
	if keepComfy && drainComfy == "" {
		return fmt.Errorf("--keep-comfyui-up needs --drain-comfyui, the ComfyUI base URL to unload on drain")
	}
	return nil
}
