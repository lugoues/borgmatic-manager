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

	// borgmatic binary and version floor.
	borgmaticPath := ""
	if path, err := resolveBorgmatic(e.cfg); err != nil {
		r.fail("borgmatic", err.Error())
	} else {
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		version, err := commandOutput(cctx, path, "--version")
		cancel()
		switch {
		case err != nil:
			r.fail("borgmatic", fmt.Sprintf("running %s --version: %v", path, err))
		case !versionAtLeast(strings.TrimSpace(version), minBorgmatic):
			r.fail("borgmatic", fmt.Sprintf("%s is %s, need >= %d.%d.%d (distro packages often lag; use uv or pipx)",
				path, strings.TrimSpace(version), minBorgmatic[0], minBorgmatic[1], minBorgmatic[2]))
		default:
			borgmaticPath = path
			r.pass("borgmatic", fmt.Sprintf("%s (%s)", path, strings.TrimSpace(version)))
		}
	}

	// borg: hard floor only when snapshot hooks are configured.
	snapshots := snapshotHooksConfigured(e.cfg, e.groupOverrides)
	if borgPath, err := exec.LookPath("borg"); err != nil {
		if snapshots {
			r.fail("borg", "not on PATH, and snapshot hooks are configured")
		} else {
			r.warn("borg", "not on PATH; borgmatic will fail until it is installed")
		}
	} else {
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
		out, err := commandOutput(cctx, borgPath, "--version")
		cancel()
		if fields := strings.Fields(out); err == nil && len(fields) > 0 {
			version := fields[len(fields)-1]
			switch {
			case versionAtLeast(version, minBorg):
				r.pass("borg", fmt.Sprintf("%s (%s)", borgPath, version))
			case snapshots:
				r.fail("borg", fmt.Sprintf("%s is older than %d.%d and snapshot hooks are configured: archives would record snapshot paths", version, minBorg[0], minBorg[1]))
			default:
				r.warn("borg", fmt.Sprintf("%s is older than %d.%d (fine until snapshot hooks are enabled)", version, minBorg[0], minBorg[1]))
			}
		} else {
			r.warn("borg", fmt.Sprintf("could not read version from %s", borgPath))
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
			if borgmaticPath != "" {
				r.checkGenerate(ctx, e, backupState, borgmaticPath)
			}
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
	defer os.RemoveAll(dir)

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
	if _, err := gen.Generate(backupState); err != nil {
		r.fail("generate", err.Error())
		return
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		r.pass("generate", "no group configs to validate")
		return
	}
	r.pass("generate", fmt.Sprintf("%d config(s) compiled", len(files)))

	for _, file := range files {
		group := strings.TrimSuffix(filepath.Base(file), ".yaml")
		cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
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
