package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/toolchain"
)

// doctorTimeout bounds each external command the checks run.
const doctorTimeout = 2 * time.Minute

type doctorReport struct {
	failed int
	warned int
	// sawLabelWarning suppresses the generic no-groups hint when discovery
	// already reported specific label problems.
	sawLabelWarning bool
}

// warnCapturingLogger routes a component's WARN+ records into the report as
// labeled lines; lower levels are dropped.
func warnCapturingLogger(r *doctorReport) *slog.Logger {
	return slog.New(reportHandler{r: r})
}

type reportHandler struct {
	r     *doctorReport
	attrs []slog.Attr
}

func (h reportHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h reportHandler) Handle(_ context.Context, rec slog.Record) error {
	detail := rec.Message
	appendAttr := func(a slog.Attr) bool {
		detail += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	}
	for _, a := range h.attrs {
		appendAttr(a)
	}
	rec.Attrs(appendAttr)
	h.r.sawLabelWarning = true
	if rec.Level >= slog.LevelError {
		h.r.fail("labels", detail)
	} else {
		h.r.warn("labels", detail)
	}
	return nil
}

func (h reportHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return h
}

func (h reportHandler) WithGroup(string) slog.Handler { return h }

func (r *doctorReport) pass(name, detail string) { r.line("ok", name, detail) }

func (r *doctorReport) warn(name, detail string) {
	r.warned++
	r.line("warn", name, detail)
}

func (r *doctorReport) fail(name, detail string) {
	r.failed++
	r.line("FAIL", name, detail)
}

func (r *doctorReport) line(verdict, name, detail string) {
	if detail != "" {
		fmt.Printf("%-4s  %-16s %s\n", verdict, name, detail)
		return
	}
	fmt.Printf("%-4s  %s\n", verdict, name)
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the setup: config, socket, borgmatic/borg, labels, generated configs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runDoctor(cmd.Context())
		},
	}
}

// runDoctor runs every setup check it can, reporting all findings instead of
// stopping at the first: one pass should surface every problem.
func runDoctor(ctx context.Context) error {
	r := &doctorReport{}
	fmt.Println()

	// Config: parse, conf.d merge, group overlays, durations.
	e, err := loadEnv()
	if err != nil {
		r.fail("config", err.Error())
		fmt.Println()
		return fmt.Errorf("%d check(s) failed", r.failed)
	}
	period, err := e.cfg.ParsedPeriod()
	if err != nil {
		r.fail("config", err.Error())
	} else if _, err := runTimeoutFromConfig(e.cfg); err != nil {
		r.fail("config", err.Error())
	} else {
		detail := fmt.Sprintf("%s, period %s", filepath.Join(e.configDir, "manager.yaml"), period)
		if len(e.cfg.GroupPeriods) > 0 {
			detail += fmt.Sprintf(", %d group period override(s)", len(e.cfg.GroupPeriods))
		}
		r.pass("config", detail)
	}

	// Container socket.
	socketOK := true
	if _, err := e.rt.ListVolumes(ctx); err != nil {
		socketOK = false
		r.fail("socket", fmt.Sprintf("%s: %v (set CONTAINER_SOCKET to override)", e.rt.SocketPath(), err))
	} else {
		r.pass("socket", e.rt.SocketPath())
	}

	// borgmatic binary and version floor. resolveBorgmatic is the daemon's
	// own selection: the explicit override, else the managed toolchain,
	// provisioned on demand right here. Doctor validates the setup the
	// daemon will actually run, provisioning included: a failure below is
	// the same failure the daemon hits, so it is a fail, never a prediction.
	borgmaticPath := ""
	if path, err := resolveBorgmatic(ctx, e.cfg, e.toolchainDir()); err != nil {
		r.fail("borgmatic", err.Error())
	} else {
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		version, err := commandOutput(cctx, path, "--version")
		cancel()
		switch {
		case err != nil:
			r.fail("borgmatic", fmt.Sprintf("running %s --version: %v", path, err))
		case !versionAtLeast(borgmaticVersionOf(version), minBorgmatic):
			// Only an explicit override can sit below the floor: the
			// toolchain's borgmatic is pinned above it.
			r.fail("borgmatic", fmt.Sprintf("%s is %s, need >= %d.%d.%d (unset manager.borgmatic_path / BORGMATIC_PATH to use the managed toolchain)",
				path, strings.TrimSpace(version), minBorgmatic[0], minBorgmatic[1], minBorgmatic[2]))
		default:
			borgmaticPath = path
			r.pass("borgmatic", fmt.Sprintf("%s (%s)", path, strings.TrimSpace(version)))
		}
	}

	// Which borgmatic is in use and whether the manager's own toolchain backs
	// it: the first question when "it works in my shell but not in the unit".
	// Judged by the path actually selected above, not the manifest: without
	// the explicit override the toolchain is the only possible selection, so
	// anything else here means the selection above already failed.
	tc := toolchain.New(e.toolchainDir(), slog.New(slog.DiscardHandler))
	selectedToolchain := borgmaticPath != "" && strings.HasPrefix(borgmaticPath, e.toolchainDir()+string(filepath.Separator))
	switch {
	case explicitBorgmaticPath(e.cfg) != "":
		r.pass("toolchain", "not in use: manager.borgmatic_path / BORGMATIC_PATH overrides it")
	case selectedToolchain:
		if m, fresh, err := tc.Info(); err == nil {
			state := "current"
			if !fresh {
				state = "stale; the next launch refreshes it"
			}
			r.pass("toolchain", fmt.Sprintf("in use: borgmatic %s (uv %s, python %s), %s",
				m.BorgmaticVersion, m.UVVersion, m.PythonVersion, state))
		} else {
			r.pass("toolchain", "in use")
		}
	case tc.Exists():
		r.warn("toolchain", "provisioned but not usable (see the borgmatic line above); the next launch retries the repair")
	default:
		r.warn("toolchain", "not provisioned; provisioning on demand failed (see the borgmatic line above)")
	}

	// borg is required outright: without the engine nothing runs. The same
	// verdict as the daemon's gates, malformed global local_path included: the
	// daemon refuses to start on that, and doctor probing PATH borg instead
	// would report a passing setup the daemon rejects.
	if err := globalLocalPathError(e.cfg); err != nil {
		r.fail("borg", err.Error())
	}
	// A quiet discovery feeds the hooks scan; nil (socket down) is fine. The
	// labels section below runs its own discovery with the warning-capturing
	// logger.
	// Merged with the durable cache, exactly as preflight and the cycle see
	// it: a group whose containers are all offline keeps its cached label
	// config (a label-sourced local_path included), and diagnosing live-only
	// state would fail a configuration the daemon starts fine on.
	var discoveredState *models.BackupState
	if socketOK {
		if st, _, err := e.discoverMerged(ctx, slog.New(slog.DiscardHandler)); err == nil {
			discoveredState = st
		}
	}
	for _, bc := range borgCommands(e.cfg, e.groupOverrides, discoveredState) {
		borgPath, err := resolveBorgCommand(bc.command)
		if err != nil {
			r.fail("borg", fmt.Sprintf("%s: %v; install it from your distribution (the manager provisions borgmatic, never borg)", bc.source, err))
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		out, err := commandOutput(cctx, borgPath, "--version")
		cancel()
		if fields := strings.Fields(out); err == nil && len(fields) > 0 {
			version := fields[len(fields)-1]
			switch {
			case !toolchain.PlausibleReportedVersion(out):
				r.fail("borg", fmt.Sprintf("%s reports no usable version (output %q); the daemon refuses to start on it", borgPath, strings.TrimSpace(out)))
			case versionAtLeast(version, minBorg):
				r.pass("borg", fmt.Sprintf("%s (%s, %s)", borgPath, version, bc.source))
			case bc.snapshots:
				r.fail("borg", fmt.Sprintf("%s (%s) is older than %d.%d and a snapshot-hook group uses it: archives would record snapshot paths", version, bc.source, minBorg[0], minBorg[1]))
			default:
				r.warn("borg", fmt.Sprintf("%s (%s) is older than %d.%d (fine until snapshot hooks are enabled)", version, bc.source, minBorg[0], minBorg[1]))
			}
		} else {
			// The daemon refuses to start on this; doctor saying "no
			// failures" while preflight fails would be a lie.
			r.fail("borg", fmt.Sprintf("running %s --version failed (%s): %v", borgPath, bc.source, err))
		}
	}

	// Container CLI for generated dump commands.
	if cli := detectContainerCLI(e.cfg, e.rt.SocketPath()); cli == "" {
		r.warn("container-cli", "neither docker nor podman on PATH; database dump commands will fail")
	} else {
		r.pass("container-cli", cli)
	}

	// Discovery: label parsing and volume checks against the live socket.
	// Warnings discovery logs (near-miss labels, skipped volumes) become
	// report lines instead of interleaved log output.
	if socketOK {
		backupState, err := discovery.Discover(ctx, e.rt, warnCapturingLogger(r))
		if err != nil {
			r.fail("labels", err.Error())
		} else {
			groups := 0
			for _, g := range backupState.Groups {
				if len(g.Volumes) > 0 || len(g.Databases) > 0 {
					groups++
				}
			}
			if groups > 0 {
				r.pass("labels", fmt.Sprintf("%d group(s) discovered", groups))
			} else if !r.sawLabelWarning {
				r.warn("labels", "no backup groups discovered; check container labels (see: borgmatic-manager discover)")
			}

			// Generation and borgmatic's own schema validation, in a throwaway dir.
			// Not gated on borgmatic being installed: label and config-shape
			// problems are exactly what an operator is running doctor for, and
			// they are diagnosable without it. Only the `config validate`
			// sub-step needs the binary, and it says so when it is missing.
			//
			// The merged (cache-included) state when available, live otherwise:
			// an offline group's cached label config (a broken local_path
			// included) is judged by every cycle, and doctor diagnosing the
			// live-only view would miss what those cycles refuse.
			genState := backupState
			if discoveredState != nil {
				genState = discoveredState
			}
			r.checkGenerate(ctx, e, genState, borgmaticPath)
		}
	}

	fmt.Println()
	if r.failed > 0 {
		return fmt.Errorf("%d check(s) failed", r.failed)
	}
	if r.warned > 0 {
		fmt.Printf("no failures (%d warning(s))\n\n", r.warned)
	} else {
		fmt.Print("all checks passed\n\n")
	}
	return nil
}

// checkGenerate compiles configs into a throwaway private dir and runs
// borgmatic's own `config validate` over each, so label/config typos surface
// here instead of one failed cycle at a time.
func (r *doctorReport) checkGenerate(ctx context.Context, e *env, backupState *models.BackupState, borgmaticPath string) {
	dir, err := e.privateConfigDir("doctor")
	if err != nil {
		r.fail("generate", err.Error())
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Refusals are reported below; the generator's own logging would repeat them.
	gen := e.newGenerator(dir, slog.New(slog.DiscardHandler))
	_, refusals, err := gen.Plan(backupState)
	if err != nil {
		r.fail("generate", err.Error())
		return
	}
	for _, refusal := range refusals {
		r.warn("generate", fmt.Sprintf("group %s refused: %s", refusal.Group, refusal.Reason))
	}
	if _, _, err = gen.Generate(backupState); err != nil {
		r.fail("generate", err.Error())
		return
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		r.pass("generate", "no group configs to validate")
		return
	}
	r.pass("generate", fmt.Sprintf("%d config(s) compiled", len(files)))
	r.validateGeneratedConfigs(ctx, files, borgmaticPath)
}

// validateGeneratedConfigs runs borgmatic's own schema check over each compiled
// config. Everything before this point is the manager's own doing and needs no
// borgmatic; only borgmatic can judge borgmatic's schema, so an absent binary
// skips this step loudly rather than silently shortening the report.
func (r *doctorReport) validateGeneratedConfigs(ctx context.Context, files []string, borgmaticPath string) {
	if borgmaticPath == "" {
		r.warn("validate", "skipped: borgmatic is not installed, so its schema check cannot run (the configs above still compiled)")
		return
	}

	for _, file := range files {
		group := strings.TrimSuffix(filepath.Base(file), ".yaml")
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		// #nosec G204 -- borgmaticPath is the resolved borgmatic binary; running it is this program's purpose
		out, err := exec.CommandContext(cctx, borgmaticPath, "--config", file, "config", "validate").CombinedOutput()
		cancel()
		if err != nil {
			detail := strings.TrimSpace(string(out))
			if len(detail) > 200 {
				detail = detail[:200] + "…"
			}
			r.fail("validate:"+group, detail)
			continue
		}
		r.pass("validate:"+group, "")
	}
}
