package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// actionCreate is the borgmatic action that produces a --json result.
// defaultActions include prune/compact/check: create alone would never prune.
const actionCreate = "create"

var defaultActions = []string{actionCreate, "prune", "compact", "check"}

// defaultKillGrace is the SIGTERM-to-SIGKILL grace after a run timeout fires.
const defaultKillGrace = 60 * time.Second

// snapshotLockKey serializes groups with snapshot hooks: borgmatic's snapshot
// cleanup is name-prefix-matched, so concurrent runs destroy each other's snapshots.
const snapshotLockKey = "snapshots"

// Runner executes borgmatic on the host for backup groups.
type Runner struct {
	logger        *slog.Logger
	configDir     string
	borgmaticPath string
	actions       []string
	runTimeout    time.Duration
	killGrace     time.Duration

	// locks holds named binary semaphores: "group:<name>" (try), "repo:<key>" and "snapshots" (blocking, ordered).
	locks   map[string]chan struct{}
	locksMu sync.Mutex

	// bootstrapHinted dedupes the guided repo-create hint to once per group.
	bootstrapHinted sync.Map

	// execCommand is an exec.Command seam for testing.
	execCommand func(ctx context.Context, name string, args ...string) *exec.Cmd

	// recorder, when set, receives every run's outcome for status display.
	recorder Recorder
}

// Recorder persists run outcomes; *state.ScheduleStore implements it.
type Recorder interface {
	RecordRun(group string, outcome state.RunOutcome)
}

// SetRecorder wires run-outcome persistence (nil disables it).
func (r *Runner) SetRecorder(rec Recorder) {
	r.recorder = rec
}

// NewRunner creates a Runner. borgmaticPath must be a resolved binary path;
// actions defaults to create/prune/compact/check when empty; runTimeout of 0
// means no per-run timeout.
func NewRunner(logger *slog.Logger, configDir, borgmaticPath string, actions []string, runTimeout time.Duration) *Runner {
	if len(actions) == 0 {
		actions = defaultActions
	}
	return &Runner{
		logger:        logger,
		configDir:     configDir,
		borgmaticPath: borgmaticPath,
		actions:       actions,
		runTimeout:    runTimeout,
		killGrace:     defaultKillGrace,
		locks:         make(map[string]chan struct{}),
		execCommand: func(_ context.Context, name string, args ...string) *exec.Cmd {
			// Not CommandContext: cancellation must SIGTERM the process group
			// (borg releases repo locks), never SIGKILL outright.
			return exec.Command(name, args...) // #nosec G204 -- executing the resolved borgmatic binary is this program's purpose
		},
	}
}

// sem returns the named binary semaphore, creating it lazily.
func (r *Runner) sem(key string) chan struct{} {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	s, ok := r.locks[key]
	if !ok {
		s = make(chan struct{}, 1)
		r.locks[key] = s
	}
	return s
}

// TryRunGroup runs a backup for the group, returning (false, nil) if an
// overlapping cycle already holds it. Snapshot and repo locks are then taken
// blocking in one global order: groups sharing a repo serialize, not skip.
func (r *Runner) TryRunGroup(ctx context.Context, groupName string, meta config.GroupRunMeta) (bool, error) {
	groupSem := r.sem("group:" + groupName)
	select {
	case groupSem <- struct{}{}:
	default:
		return false, nil
	}
	defer func() { <-groupSem }()

	// A single global lock order (snapshots, then sorted repos) prevents deadlock.
	var keys []string
	if meta.SnapshotHooks {
		keys = append(keys, snapshotLockKey)
	}
	repos := append([]string(nil), meta.Repos...)
	sort.Strings(repos)
	for _, repo := range repos {
		keys = append(keys, "repo:"+repo)
	}

	var held []chan struct{}
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			<-held[i]
		}
	}
	for _, key := range keys {
		s := r.sem(key)
		select {
		case s <- struct{}{}:
			held = append(held, s)
		case <-ctx.Done():
			release()
			return true, ctx.Err()
		}
	}
	defer release()

	return true, r.runGroup(ctx, groupName)
}

// runGroup validates the group's generated config, then executes borgmatic.
func (r *Runner) runGroup(ctx context.Context, groupName string) error {
	configPath := filepath.Join(r.configDir, groupName+".yaml")

	if err := r.validateConfig(ctx, groupName, configPath); err != nil {
		return err
	}

	// create --json puts a machine-readable result on stdout without disturbing --log-json.
	args := []string{"--config", configPath, "--verbosity", "1", "--log-json"}
	for _, action := range r.actions {
		args = append(args, action)
		if action == actionCreate {
			args = append(args, "--json")
		}
	}
	cmd := r.execCommand(ctx, r.borgmaticPath, args...)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Own process group: borgmatic's shutdown signal fan-out must not hit the manager.
	cmd.SysProcAttr.Setpgid = true

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe for group %s: %w", groupName, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe for group %s: %w", groupName, err)
	}

	r.logger.Info("starting borgmatic", "group", groupName, "actions", strings.Join(r.actions, ","))
	start := time.Now()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting borgmatic for group %s: %w", groupName, err)
	}

	run := &runState{logger: r.logger, group: groupName}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run.consume(stdout, "stdout") }()
	go func() { defer wg.Done(); run.consume(stderr, "stderr") }()

	// Shutdown forwards SIGTERM and waits (systemd's TimeoutStopSec backstops);
	// a run timeout escalates to SIGKILL so a wedged child can't hold locks forever.
	done := make(chan struct{})
	var timedOut atomic.Bool
	go func() {
		var timeoutCh <-chan time.Time
		if r.runTimeout > 0 {
			t := time.NewTimer(r.runTimeout)
			defer t.Stop()
			timeoutCh = t.C
		}
		select {
		case <-done:
		case <-ctx.Done():
			r.logger.Info("shutdown: signalling borgmatic", "group", groupName)
			signalGroup(cmd, syscall.SIGTERM)
		case <-timeoutCh:
			timedOut.Store(true)
			r.logger.Error("run timeout exceeded: signalling borgmatic", "group", groupName, "timeout", r.runTimeout)
			signalGroup(cmd, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(r.killGrace):
				r.logger.Error("borgmatic ignored SIGTERM: killing process group", "group", groupName)
				signalGroup(cmd, syscall.SIGKILL)
			}
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(done)

	return r.interpretResult(groupName, configPath, waitErr, run, time.Since(start), timedOut.Load())
}

// validateConfig runs 'borgmatic config validate' as a per-cycle gate,
// converting schema drift between the manager and borgmatic into a precise
// failure instead of a broken backup run.
func (r *Runner) validateConfig(ctx context.Context, groupName, configPath string) error {
	cmd := r.execCommand(ctx, r.borgmaticPath, "--config", configPath, "config", "validate")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.logger.Error("generated config failed borgmatic validation; skipping group this cycle",
			"group", groupName, "config", configPath, "output", strings.TrimSpace(string(out)))
		return fmt.Errorf("config validation failed for group %s", groupName)
	}
	return nil
}

// interpretResult turns exit state into logs and an error. borgmatic exits 0
// even with warnings (output-only), 1 on error, 143 on SIGTERM.
func (r *Runner) interpretResult(groupName, configPath string, waitErr error, run *runState, duration time.Duration, timedOut bool) error {
	warnings := run.warnings.Load()
	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("waiting for borgmatic for group %s: %w", groupName, waitErr)
		}
	}

	record := func(result string) {
		if r.recorder == nil {
			return
		}
		outcome := state.RunOutcome{
			Finished:        time.Now(),
			Result:          result,
			ExitCode:        exitCode,
			Warnings:        warnings,
			DurationSeconds: int64(duration.Seconds()),
			Archive:         run.archiveName(),
		}
		if res := run.parseCreateResult(); res != nil {
			outcome.Archive = res.Archive.Name
			outcome.Files = res.Archive.Stats.NFiles
			outcome.OriginalBytes = res.Archive.Stats.OriginalSize
			outcome.CompressedBytes = res.Archive.Stats.CompressedSize
			outcome.DeduplicatedBytes = res.Archive.Stats.DeduplicatedSize
		}
		r.recorder.RecordRun(groupName, outcome)
	}

	switch exitCode {
	case 0:
		record("ok")
		r.logger.Info("borgmatic finished", "group", groupName, "exit_code", exitCode,
			"warnings", warnings, "duration", duration.Round(time.Second).String())
		return nil

	case 143, 130:
		record("terminated")
		r.logger.Warn("borgmatic terminated by signal", "group", groupName, "exit_code", exitCode,
			"timed_out", timedOut, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s terminated (exit %d)", groupName, exitCode)

	default:
		record("failed")
		if run.repoMissing.Load() {
			if _, hinted := r.bootstrapHinted.LoadOrStore(groupName, struct{}{}); !hinted {
				r.logger.Error("repository does not exist, initialize it once, then backups proceed on the next cycle",
					"group", groupName,
					"hint", fmt.Sprintf("borgmatic-manager borgmatic %s repo-create --encryption repokey-blake2", groupName))
			}
		}
		r.logger.Error("borgmatic failed", "group", groupName, "exit_code", exitCode,
			"warnings", warnings, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s failed (exit %d)", groupName, exitCode)
	}
}

// runState accumulates per-run output facts across both stream consumers.
type runState struct {
	logger      *slog.Logger
	group       string
	warnings    atomic.Int64
	repoMissing atomic.Bool

	// archive is the archive name borg reported; resultBuf accumulates non-log
	// stdout (the create --json result). Both guarded.
	archiveMu sync.Mutex
	archive   string
	resultBuf strings.Builder
}

// maxResultBuf bounds buffered non-log stdout; the create --json result is small.
const maxResultBuf = 1 << 20

func (rs *runState) bufferResult(line string) {
	rs.archiveMu.Lock()
	defer rs.archiveMu.Unlock()
	if rs.resultBuf.Len()+len(line) <= maxResultBuf {
		rs.resultBuf.WriteString(line)
		rs.resultBuf.WriteByte('\n')
	}
}

// createResult mirrors one repository entry of create --json output; the first
// entry is representative across repositories.
type createResult struct {
	Archive struct {
		Name  string `json:"name"`
		Stats struct {
			NFiles           int64 `json:"nfiles"`
			OriginalSize     int64 `json:"original_size"`
			CompressedSize   int64 `json:"compressed_size"`
			DeduplicatedSize int64 `json:"deduplicated_size"`
		} `json:"stats"`
	} `json:"archive"`
}

// parseCreateResult stream-decodes buffered stdout (the result can arrive
// concatenated with log records) and returns the first create result.
func (rs *runState) parseCreateResult() *createResult {
	rs.archiveMu.Lock()
	raw := rs.resultBuf.String()
	rs.archiveMu.Unlock()

	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var doc json.RawMessage
		if err := dec.Decode(&doc); err != nil {
			return nil
		}
		var results []createResult
		if err := json.Unmarshal(doc, &results); err == nil && len(results) > 0 && results[0].Archive.Name != "" {
			return &results[0]
		}
	}
}

func (rs *runState) setArchive(name string) {
	rs.archiveMu.Lock()
	rs.archive = name
	rs.archiveMu.Unlock()
}

func (rs *runState) archiveName() string {
	rs.archiveMu.Lock()
	defer rs.archiveMu.Unlock()
	return rs.archive
}

// borgmaticLogRecord is one --log-json line (borgmatic's own records and
// Borg passthrough share this shape).
type borgmaticLogRecord struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Levelname string `json:"levelname"`
	Name      string `json:"name"`
}

// consume re-emits JSON log records at their level, forwarding raw lines
// otherwise. Non-JSON stderr counts as a warning (borgmatic routes WARNING+ there).
func (rs *runState) consume(stream io.Reader, name string) {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		rs.emit(line, name)
	}
	if err := scanner.Err(); err != nil {
		// Keep draining to EOF on scanner errors: a full pipe would block
		// borgmatic, which would hold its repo locks forever.
		rs.warnings.Add(1)
		rs.logger.Warn("borgmatic output overflowed the line scanner; draining remaining output unparsed",
			"group", rs.group, "stream", name, "error", err)
		_, _ = io.Copy(io.Discard, stream)
	}
}

func (rs *runState) emit(line, stream string) {
	var rec borgmaticLogRecord
	if err := json.Unmarshal([]byte(line), &rec); err == nil && rec.Levelname != "" {
		rs.checkMessage(rec.Message)
		switch rec.Levelname {
		case "CRITICAL", "ERROR":
			rs.logger.Error(rec.Message, "group", rs.group, "source", rec.Name)
		case "WARNING":
			rs.warnings.Add(1)
			rs.logger.Warn(rec.Message, "group", rs.group, "source", rec.Name)
		case "DEBUG":
			rs.logger.Debug(rec.Message, "group", rs.group, "source", rec.Name)
		default:
			rs.logger.Info(rec.Message, "group", rs.group, "source", rec.Name)
		}
		return
	}

	rs.checkMessage(line)
	if stream == "stderr" {
		rs.warnings.Add(1)
		rs.logger.Warn(line, "group", rs.group, "stream", stream)
		return
	}
	// Non-log JSON on stdout is the create --json result: buffer it, don't echo to the journal.
	if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
		rs.bufferResult(line)
		return
	}
	rs.logger.Info(line, "group", rs.group, "stream", stream)
}

// checkMessage watches for borg's "repository does not exist" error to drive the bootstrap hint.
func (rs *runState) checkMessage(msg string) {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "repository") && strings.Contains(lower, "does not exist") {
		rs.repoMissing.Store(true)
	}
	// Borg announces `Creating archive at "<repo>::<archive>"` at INFO.
	if rest, ok := strings.CutPrefix(msg, "Creating archive at "); ok {
		rest = strings.Trim(rest, `"`)
		if i := strings.LastIndex(rest, "::"); i >= 0 {
			rest = rest[i+2:]
		}
		if rest != "" {
			rs.setArchive(rest)
		}
	}
}

// signalGroup delivers a signal to the child's process group. Negative pid
// addresses the group; Setpgid guarantees pgid == child pid.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
