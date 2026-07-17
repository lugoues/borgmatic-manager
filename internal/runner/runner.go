package runner

import (
	"bufio"
	"bytes"
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
	"github.com/lugoues/borgmatic-manager/internal/lockfile"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// actionCreate is the borgmatic action that produces a --json result.
// defaultActions include prune/compact/check: create alone would never prune.
const actionCreate = "create"

var defaultActions = []string{actionCreate, "prune", "compact", "check"}

// defaultKillGrace is the SIGTERM-to-SIGKILL grace after a run timeout fires.
const defaultKillGrace = 60 * time.Second

// Shell-convention exit codes for death by signal (128 + signal number).
const (
	sigintExit  = 130 // 128 + SIGINT
	sigkillExit = 137 // 128 + SIGKILL, the run timeout's escalation
	sigtermExit = 143 // 128 + SIGTERM, shutdown
)

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

	// lockDir, when set, enables cross-process flock coordination; empty (tests) leaves only in-process locks.
	lockDir string

	// bootstrapHinted dedupes the guided repo-create hint to once per group.
	bootstrapHinted sync.Map

	// execCommand is an exec.Command seam for testing.
	execCommand func(ctx context.Context, name string, args ...string) *exec.Cmd

	// recorder, when set, receives every run's outcome for status display.
	recorder Recorder

	// pending + reap, when set, implement dump-helper cleanup: record before spawn, reap and clear after exit.
	pending PendingTracker
	reap    ReapFunc
}

// Recorder persists run outcomes; *state.ScheduleStore implements it.
type Recorder interface {
	RecordRun(group string, outcome state.RunOutcome)
}

// PendingTracker persists in-flight run IDs so a crashed manager's orphaned
// dump helpers can be reaped at startup. *state.ScheduleStore implements it.
type PendingTracker interface {
	RecordPending(runID, group string, started time.Time)
	ClearPending(runID string)
}

// ReapFunc force-removes the dump helper containers of one run.
type ReapFunc func(ctx context.Context, runID string) ([]string, error)

// reapHelpers force-removes dump helpers still wearing this run's label (orphans
// once borgmatic exits). Fresh context: this runs on cancelled shutdown paths.
func (r *Runner) reapHelpers(groupName, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	names, err := r.reap(ctx, runID)
	if err != nil {
		// Keep the pending record: startup reconciliation retries.
		r.logger.Warn("failed to reap dump helper containers; will retry at next startup",
			"group", groupName, "run_id", runID, "error", err)
		return
	}
	if len(names) > 0 {
		r.logger.Warn("reaped orphaned dump helper containers",
			"group", groupName, "run_id", runID, "containers", strings.Join(names, ","))
	}
	r.pending.ClearPending(runID)
}

// SetRecorder wires run-outcome persistence (nil disables it).
// SetLockDir enables cross-process flock coordination; the daemon and ad-hoc runs share dir.
func (r *Runner) SetLockDir(dir string) {
	r.lockDir = dir
}

func (r *Runner) SetRecorder(rec Recorder) {
	r.recorder = rec
}

// SetHelperReaper wires dump-helper cleanup (nil disables it).
func (r *Runner) SetHelperReaper(pending PendingTracker, reap ReapFunc) {
	r.pending = pending
	r.reap = reap
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

	// Cross-process layer: same keys as non-blocking flocks, taken with the
	// in-process locks held. Held by another process means skip, never wait.
	var heldLocks []*lockfile.Lock
	releaseLocks := func() {
		for i := len(heldLocks) - 1; i >= 0; i-- {
			heldLocks[i].Release()
		}
	}
	for _, key := range keys {
		lock, acquired, err := tryCrossLock(r.lockDir, key)
		if err != nil {
			releaseLocks()
			return false, fmt.Errorf("acquiring cross-process lock %q for group %s: %w", key, groupName, err)
		}
		if !acquired {
			releaseLocks()
			r.logger.Info("skipping group: another process holds its lock", "group", groupName, "lock", key)
			return false, ErrLockedByAnotherProcess
		}
		heldLocks = append(heldLocks, lock)
	}
	defer releaseLocks()

	return true, r.runGroup(ctx, groupName, meta.RunID)
}

// runGroup validates the group's generated config, then executes borgmatic.
func (r *Runner) runGroup(ctx context.Context, groupName, runID string) error {
	configPath := filepath.Join(r.configDir, groupName+".yaml")

	if err := r.validateConfig(ctx, groupName, configPath); err != nil {
		return err
	}

	// Record pending BEFORE spawning so a crash mid-run is reaped by ID at startup.
	if r.pending != nil && r.reap != nil && runID != "" {
		r.pending.RecordPending(runID, groupName, time.Now())
		defer r.reapHelpers(groupName, runID)
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
			r.terminateGroup(cmd, done, groupName)
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(done)

	return r.interpretResult(groupName, configPath, waitErr, run, time.Since(start), timedOut.Load())
}

// validateTimeout bounds 'borgmatic config validate', which runs while holding
// every lock TryRunGroup acquired: a hang here must not stall the scheduler.
const validateTimeout = 2 * time.Minute

// validateConfig gates each cycle on 'borgmatic config validate', turning schema
// drift into a precise, recorded failure instead of a broken backup run.
func (r *Runner) validateConfig(ctx context.Context, groupName, configPath string) error {
	cmd := r.execCommand(ctx, r.borgmaticPath, "--config", configPath, "config", "validate")
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting config validation for group %s: %w", groupName, err)
	}

	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()

	timer := time.NewTimer(validateTimeout)
	defer timer.Stop()

	var interrupted bool
	select {
	case <-exited:
	case <-ctx.Done():
		interrupted = true
		r.logger.Info("shutdown: signalling config validation", "group", groupName)
		r.terminateGroup(cmd, exited, groupName)
		<-exited
	case <-timer.C:
		r.logger.Error("config validation timed out: signalling borgmatic", "group", groupName, "timeout", validateTimeout)
		r.terminateGroup(cmd, exited, groupName)
		<-exited
	}
	err := waitErr

	// A validation we killed says nothing about the config; recording
	// config-invalid would leave the group falsely marked broken.
	if interrupted {
		return fmt.Errorf("config validation for group %s interrupted: %w", groupName, ctx.Err())
	}

	if err != nil {
		r.logger.Error("generated config failed borgmatic validation; skipping group this cycle",
			"group", groupName, "config", configPath, "output", strings.TrimSpace(out.String()))
		r.recordValidationFailure(groupName)
		return fmt.Errorf("config validation failed for group %s", groupName)
	}
	return nil
}

// terminateGroup SIGTERMs the process group, escalating to SIGKILL after the kill
// grace. Backup shutdown skips this so Borg can release repo locks cleanly.
func (r *Runner) terminateGroup(cmd *exec.Cmd, exited <-chan struct{}, groupName string) {
	signalGroup(cmd, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(r.killGrace):
		r.logger.Error("process ignored SIGTERM: killing process group", "group", groupName)
		signalGroup(cmd, syscall.SIGKILL)
	}
}

// recordValidationFailure surfaces validation failures in status instead of a stale green last run.
func (r *Runner) recordValidationFailure(groupName string) {
	if r.recorder == nil {
		return
	}
	r.recorder.RecordRun(groupName, state.RunOutcome{
		Finished: time.Now(),
		Result:   "config-invalid",
	})
}

// interpretResult turns exit state into logs and an error. borgmatic exits 0
// even with warnings (output-only), 1 on error, 143 on SIGTERM.
func (r *Runner) interpretResult(groupName, configPath string, waitErr error, run *runState, duration time.Duration, timedOut bool) error {
	warnings := run.warnings.Load()
	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			// Signal deaths report ExitCode -1; map to the shell's 128+signal
			// convention so our own terminations are not "failed (exit -1)".
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				exitCode = 128 + int(status.Signal())
			}
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
		if result != state.ResultOK {
			outcome.LastError = run.firstError()
		}
		outcome.LogTail = run.logSnapshot()
		if res := run.parseCreateResult(); res != nil {
			outcome.Archive = res.Archive.Name
			outcome.Files = res.Archive.Stats.NFiles
			outcome.OriginalBytes = res.Archive.Stats.OriginalSize
			outcome.CompressedBytes = res.Archive.Stats.CompressedSize
			outcome.DeduplicatedBytes = res.Archive.Stats.DeduplicatedSize
		}
		r.recorder.RecordRun(groupName, outcome)
	}

	switch {
	case exitCode == 0:
		record(state.ResultOK)
		r.logger.Info("borgmatic finished", "group", groupName, "exit_code", exitCode,
			"warnings", warnings, "duration", duration.Round(time.Second).String())
		return nil

	// Our own run-timeout escalation: a deliberate stop, recorded as terminated.
	case timedOut && (exitCode == sigtermExit || exitCode == sigkillExit):
		record(state.ResultTerminated)
		r.logger.Warn("borgmatic timed out and was terminated", "group", groupName, "exit_code", exitCode,
			"timeout", r.runTimeout, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s timed out after %s and was terminated", groupName, r.runTimeout)

	// SIGINT/SIGTERM without a timeout: clean shutdown, expected, not a failure.
	case exitCode == sigintExit || exitCode == sigtermExit:
		record(state.ResultTerminated)
		r.logger.Warn("borgmatic terminated by signal", "group", groupName, "exit_code", exitCode,
			"duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s terminated (exit %d)", groupName, exitCode)

	// External SIGKILL (OOM killer, kill -9) counts as failed: "terminated"
	// would hide the group from status's failed-groups alert.
	case exitCode == sigkillExit:
		record(state.ResultFailed)
		r.logger.Error("borgmatic killed (SIGKILL), likely the OOM killer or an external kill -9", "group", groupName,
			"exit_code", exitCode, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s was killed (exit %d); not a manager timeout, check for OOM", groupName, exitCode)

	default:
		record(state.ResultFailed)
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

	// firstErr keeps the first CRITICAL/ERROR message (the cause); guarded, first wins.
	errMu    sync.Mutex
	firstErr string

	// logTail is a bounded, oldest-first tail of log lines for inspect; guarded.
	logMu   sync.Mutex
	logTail []string

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

// maxReasonLen bounds a stored failure reason; the full text stays in the journal.
const maxReasonLen = 200

// recordError keeps the first non-empty message: the first failure is the cause.
func (rs *runState) recordError(msg string) {
	msg = truncateReason(msg)
	if msg == "" {
		return
	}
	rs.errMu.Lock()
	defer rs.errMu.Unlock()
	if rs.firstErr == "" {
		rs.firstErr = msg
	}
}

func (rs *runState) firstError() string {
	rs.errMu.Lock()
	defer rs.errMu.Unlock()
	return rs.firstErr
}

// maxLogTail bounds lines kept for inspect; the full log lives in the journal.
const maxLogTail = 200

func (rs *runState) recordLine(level, msg string) {
	line := truncateReason(level + " " + msg)
	if line == "" {
		return
	}
	rs.logMu.Lock()
	defer rs.logMu.Unlock()
	rs.logTail = append(rs.logTail, line)
	if len(rs.logTail) > maxLogTail {
		rs.logTail = rs.logTail[len(rs.logTail)-maxLogTail:]
	}
}

func (rs *runState) logSnapshot() []string {
	rs.logMu.Lock()
	defer rs.logMu.Unlock()
	if len(rs.logTail) == 0 {
		return nil
	}
	out := make([]string, len(rs.logTail))
	copy(out, rs.logTail)
	return out
}

// truncateReason collapses whitespace to one line and bounds length, rune-safely.
func truncateReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxReasonLen {
		return string(r[:maxReasonLen]) + "…"
	}
	return s
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
			rs.recordError(rec.Message)
			rs.recordLine(rec.Levelname, rec.Message)
			rs.logger.Error(rec.Message, "group", rs.group, "source", rec.Name)
		case "WARNING":
			rs.warnings.Add(1)
			rs.recordLine(rec.Levelname, rec.Message)
			rs.logger.Warn(rec.Message, "group", rs.group, "source", rec.Name)
		case "DEBUG":
			// Debug is journal-only noise; the inspect tail skips it.
			rs.logger.Debug(rec.Message, "group", rs.group, "source", rec.Name)
		default:
			rs.recordLine(rec.Levelname, rec.Message)
			rs.logger.Info(rec.Message, "group", rs.group, "source", rec.Name)
		}
		return
	}

	rs.checkMessage(line)
	if stream == "stderr" {
		rs.warnings.Add(1)
		rs.recordLine("WARNING", line)
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
