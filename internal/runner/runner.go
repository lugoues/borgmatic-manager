package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
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
// actionsInclude reports whether this runner's configured action set contains
// the named borgmatic action.
func (r *Runner) actionsInclude(name string) bool {
	for _, a := range r.actions {
		if a == name {
			return true
		}
	}
	return false
}

// defaultActions include prune/compact/check: create alone would never prune.
const (
	actionCreate = "create"
	actionPrune  = "prune"
	actionCheck  = "check"
)

var defaultActions = []string{actionCreate, actionPrune, "compact", actionCheck}

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
	// validateTimeout is a field only so tests can shorten it; production always
	// takes defaultValidateTimeout.
	validateTimeout time.Duration
	killGrace       time.Duration

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
		logger:          logger,
		configDir:       configDir,
		borgmaticPath:   borgmaticPath,
		actions:         actions,
		runTimeout:      runTimeout,
		validateTimeout: defaultValidateTimeout,
		killGrace:       defaultKillGrace,
		locks:           make(map[string]chan struct{}),
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

	return true, r.runGroup(ctx, groupName, meta)
}

// runGroup validates the group's generated config, then executes borgmatic.
func (r *Runner) runGroup(ctx context.Context, groupName string, meta config.GroupRunMeta) error {
	configPath := filepath.Join(r.configDir, groupName+".yaml")
	runID := meta.RunID

	if err := r.validateConfig(ctx, groupName, configPath, meta.Repositories); err != nil {
		return err
	}

	// Record pending BEFORE spawning so a crash mid-run is reaped by ID at startup.
	if r.pending != nil && r.reap != nil && runID != "" {
		// Per-run liveness lock, kernel-dropped on crash, lets the startup reaper
		// tell a live run from an orphan. Failing to take it must not fail the backup.
		if r.lockDir != "" {
			if lock, _, err := lockfile.TryExclusive(PendingLockPath(r.lockDir, runID)); err != nil {
				// A failed TryExclusive leaves the file present but unheld, which reads
				// as a dead owner: remove it to fall back to the PID check, which keeps us.
				_ = os.Remove(PendingLockPath(r.lockDir, runID))
				r.logger.Warn("cannot take pending-run liveness lock; falling back to PID protection for this run",
					"run_id", runID, "error", err)
			} else if lock != nil {
				// LIFO: these run after reapHelpers, so clear record, release, unlink,
				// keeping the invariant that a visible record's lock file is always held.
				defer func() { _ = os.Remove(PendingLockPath(r.lockDir, runID)) }()
				defer lock.Release()
			}
		}
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

	return r.interpretResult(ctx, groupName, configPath, meta.Repositories,
		newArchiveScope(meta),
		waitErr, run, start, time.Since(start), timedOut.Load())
}

// defaultValidateTimeout bounds 'borgmatic config validate', which runs while
// holding every lock TryRunGroup acquired: a hang here must not stall the
// scheduler.
const defaultValidateTimeout = 2 * time.Minute

// validateConfig gates each cycle on 'borgmatic config validate', turning schema
// drift into a precise, recorded failure instead of a broken backup run.
func (r *Runner) validateConfig(ctx context.Context, groupName, configPath string, repos []config.RepoRef) error {
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

	timer := time.NewTimer(r.validateTimeout)
	defer timer.Stop()

	var interrupted, timedOut bool
	select {
	case <-exited:
	case <-ctx.Done():
		interrupted = true
		r.logger.Info("shutdown: signalling config validation", "group", groupName)
		r.terminateGroup(cmd, exited, groupName)
		<-exited
	case <-timer.C:
		timedOut = true
		r.logger.Error("config validation timed out: signalling borgmatic", "group", groupName, "timeout", r.validateTimeout)
		r.terminateGroup(cmd, exited, groupName)
		<-exited
	}
	err := waitErr

	// A validation we killed says nothing about the config; recording
	// config-invalid would leave the group falsely marked broken.
	if interrupted {
		return fmt.Errorf("config validation for group %s interrupted: %w", groupName, ctx.Err())
	}
	// The same reasoning for a validation that hung: the exit status is ours, not
	// borgmatic's verdict. Recorded as terminated so status shows the group did
	// not run, rather than as config-invalid, which sends an operator to look for
	// a schema error that may not exist.
	if timedOut {
		r.recordValidationTimeout(groupName, repos)
		return fmt.Errorf("config validation for group %s timed out after %s", groupName, r.validateTimeout)
	}

	if err != nil {
		r.logger.Error("generated config failed borgmatic validation; skipping group this cycle",
			"group", groupName, "config", configPath, "output", strings.TrimSpace(out.String()))
		r.recordValidationFailure(groupName, repos)
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

// recordValidationFailure surfaces validation failures in status instead of a
// stale green last run.
//
// The configured repositories go with it. A config that fails schema validation
// still names its destinations (the manager wrote them), so this is not the
// "nothing to attribute it to" case: without them the run counts once under an
// empty repository, which drops it out of every repository-filtered dashboard,
// and the inventory cannot reconcile a destination added or removed in the same
// edit that broke the config.
// recordValidationTimeout records a validation the manager killed. The group did
// not run and its config was never judged, so per-repository state is left
// untouched: an unfinished validation confirms nothing about any destination.
func (r *Runner) recordValidationTimeout(groupName string, repos []config.RepoRef) {
	if r.recorder == nil {
		return
	}
	outcome := state.RunOutcome{
		Finished: time.Now(),
		Result:   state.ResultTerminated,
		// The actions are what this run would have taken; the validation dying
		// says nothing about that. Without it the run drops out of the
		// per-repository counter as though it were a maintenance cycle.
		CreateAttempted:   r.actionsInclude(actionCreate),
		RepositoriesKnown: true,
	}
	for _, ref := range repos {
		id := refID(ref)
		outcome.ConfiguredRepositories = append(outcome.ConfiguredRepositories, id)
		if ref.Path != "" {
			if outcome.ConfiguredRepositoryPaths == nil {
				outcome.ConfiguredRepositoryPaths = map[string]string{}
			}
			outcome.ConfiguredRepositoryPaths[id] = resolvedRepoPath(ref)
		}
	}
	r.recorder.RecordRun(groupName, outcome)
}

func (r *Runner) recordValidationFailure(groupName string, repos []config.RepoRef) {
	if r.recorder == nil {
		return
	}
	outcome := state.RunOutcome{
		Finished:          time.Now(),
		Result:            "config-invalid",
		CreateAttempted:   r.actionsInclude(actionCreate),
		RepositoriesKnown: true,
	}
	for _, ref := range repos {
		id := refID(ref)
		outcome.ConfiguredRepositories = append(outcome.ConfiguredRepositories, id)
		if ref.Path != "" {
			if outcome.ConfiguredRepositoryPaths == nil {
				outcome.ConfiguredRepositoryPaths = map[string]string{}
			}
			outcome.ConfiguredRepositoryPaths[id] = resolvedRepoPath(ref)
		}
	}
	r.recorder.RecordRun(groupName, outcome)
}

// interpretResult turns exit state into logs and an error. borgmatic exits 0
// even with warnings (output-only), 1 on error, 143 on SIGTERM.
func (r *Runner) interpretResult(ctx context.Context, groupName, configPath string, repos []config.RepoRef, scope archiveScope, waitErr error, run *runState, start time.Time, duration time.Duration, timedOut bool) error {
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

	results := run.parseCreateResults()
	record := func(result string, repoOutcomes []state.RepoOutcome) {
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
		if len(results) > 0 {
			rep := results[0] // representative for group-level dataset size
			outcome.Archive = rep.Archive.Name
			outcome.Files = rep.Archive.Stats.NFiles
			outcome.OriginalBytes = rep.Archive.Stats.OriginalSize
			outcome.CompressedBytes = rep.Archive.Stats.CompressedSize
			outcome.DeduplicatedBytes = rep.Archive.Stats.DeduplicatedSize
		}
		outcome.Repositories = repoOutcomes
		outcome.CreateAttempted = r.actionsInclude(actionCreate)
		outcome.RepositoriesKnown = true
		for _, ref := range repos {
			id := refID(ref)
			outcome.ConfiguredRepositories = append(outcome.ConfiguredRepositories, id)
			if ref.Path != "" {
				if outcome.ConfiguredRepositoryPaths == nil {
					outcome.ConfiguredRepositoryPaths = map[string]string{}
				}
				outcome.ConfiguredRepositoryPaths[id] = resolvedRepoPath(ref)
			}
		}
		r.recorder.RecordRun(groupName, outcome)
	}

	switch {
	case exitCode == 0:
		record(state.ResultOK, r.perRepoSuccess(repos, results))
		r.logger.Info("borgmatic finished", "group", groupName, "exit_code", exitCode,
			"warnings", warnings, "duration", duration.Round(time.Second).String())
		return nil

	// Our own run-timeout escalation: a deliberate stop, recorded as terminated.
	case timedOut && (exitCode == sigtermExit || exitCode == sigkillExit):
		record(state.ResultTerminated, measuredOutcomes(repos, results))
		r.logger.Warn("borgmatic timed out and was terminated", "group", groupName, "exit_code", exitCode,
			"timeout", r.runTimeout, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s timed out after %s and was terminated", groupName, r.runTimeout)

	// SIGINT/SIGTERM without a timeout: clean shutdown, expected, not a failure.
	case exitCode == sigintExit || exitCode == sigtermExit:
		record(state.ResultTerminated, measuredOutcomes(repos, results))
		r.logger.Warn("borgmatic terminated by signal", "group", groupName, "exit_code", exitCode,
			"duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s terminated (exit %d)", groupName, exitCode)

	// External SIGKILL (OOM killer, kill -9) counts as failed: "terminated"
	// would hide the group from status's failed-groups alert.
	case exitCode == sigkillExit:
		record(state.ResultFailed, r.perRepoFailure(ctx, configPath, repos, results, scope, run, start))
		r.logger.Error("borgmatic killed (SIGKILL), likely the OOM killer or an external kill -9", "group", groupName,
			"exit_code", exitCode, "duration", duration.Round(time.Second).String())
		return fmt.Errorf("borgmatic for group %s was killed (exit %d); not a manager timeout, check for OOM", groupName, exitCode)

	default:
		record(state.ResultFailed, r.perRepoFailure(ctx, configPath, repos, results, scope, run, start))
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

	// errText accumulates full CRITICAL/ERROR message bodies (untruncated, bounded
	// count) so per-repository failure attribution can find which destination a
	// failure names. Guarded.
	errTextMu    sync.Mutex
	errText      []string
	errTextBytes int

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

// refID is a repository's stable identity: its label when set, else its path.
// refID is the key a repository's persisted state is stored under.
//
// A label wins, being the operator's own name for the destination. Failing that
// it is the path, except that an environment-backed path is resolved first: the
// literal "${BORG_REPO}" is the same string whatever it points at, so a record
// stored under it survives the variable being repointed. Failures do not clear
// LastSuccess, so the new destination would inherit the old one's freshness and
// report as recently backed up having never produced an archive.
//
// Resolving means the key changes when the destination does, which is the point:
// the old record no longer matches a configured repository and is pruned, and
// the new one starts with nothing known about it.
func refID(ref config.RepoRef) string {
	if ref.Label != "" {
		return ref.Label
	}
	return resolvedRepoPath(ref)
}

// resolvedRepoPath is the destination borg will actually write to.
//
// Everywhere a path is persisted it has to be this rather than the configured
// expression, because "${BORG_REPO}" is the same string whatever it points at.
// A labelled repository keeps its id across a repoint by design, so the stored
// path is the only thing that can notice the change, and storing the unexpanded
// expression means it never does: the new destination inherits the old one's
// last success and statistics and reports as previously successful.
func resolvedRepoPath(ref config.RepoRef) string {
	if strings.Contains(ref.Path, "${") {
		if expanded := os.ExpandEnv(ref.Path); expanded != "" && expanded != ref.Path {
			return expanded
		}
	}
	return ref.Path
}

// repoIdentity is how the runner decides that two spellings name the same
// repository: canonicalized, and expanded through the environment first.
//
// borgmatic resolves ${BORG_REPO} and friends from the environment this process
// hands it, so expanding here recovers the location borg reports. Without it, a
// group whose repositories are all environment-backed has no identity to match
// on: the canonical pass skips them, the lone-leftover rule cannot pair two
// against two, and every destination is recorded successful with no stats at
// all, so the size, file count and duration gauges never appear.
//
// config.CanonicalRepoKey deliberately does not expand, because it also answers
// "which repositories must not run concurrently", where every unresolvable path
// has to keep colliding with every other so the lock stays conservative.
// Expanding is right for matching and wrong for locking.
func repoIdentity(path string) string {
	if key := config.CanonicalRepoKey(path); key != config.UnknownRepoKey {
		return key
	}
	if expanded := os.ExpandEnv(path); expanded != "" && expanded != path {
		return config.CanonicalRepoKey(expanded)
	}
	return config.UnknownRepoKey
}

// archiveScope is how the probe identifies this group's archives, and whether it
// can be trusted to.
//
// An empty pattern means the group's config names no archive format to match on;
// the probe then asks "is there a fresh archive here at all", which is the same
// question only when no other group shares the repository. Generation refuses a
// shared-repo group whose archive_name_format lacks the {group} token, so a
// shared repository always yields a pattern.
//
// ambiguous is the case a pattern alone cannot express: the pattern exists and
// is correct for retention, but another group sharing the repository has a name
// this one prefixes, so "*-app-*" also matches "*-app-prod-*". It cannot confirm
// that this group wrote anything.
type archiveScope struct {
	pattern string
	// ambiguous holds the canonical keys of the repositories where another
	// group's archives also match this pattern. Per repository, because a group
	// can share one destination with a colliding sibling and have another to
	// itself, and only the shared one is in question.
	ambiguous map[string]bool
}

// canConfirm reports whether a probe against this repository proves this group's
// backup reached it, rather than merely proving that some archive is there.
func (a archiveScope) canConfirm(repoPath string) bool {
	return !a.ambiguous[config.CanonicalRepoKey(repoPath)]
}

// anyAmbiguous reports whether any repository in this run lost confirmation, for
// the one warning emitted per run rather than per repository.
func (a archiveScope) anyAmbiguous() bool { return len(a.ambiguous) > 0 }

// newArchiveScope builds the scope for a group's run.
func newArchiveScope(meta config.GroupRunMeta) archiveScope {
	scope := archiveScope{pattern: meta.ArchivePattern}
	if len(meta.AmbiguousRepos) > 0 {
		scope.ambiguous = make(map[string]bool, len(meta.AmbiguousRepos))
		for _, key := range meta.AmbiguousRepos {
			scope.ambiguous[key] = true
		}
	}
	return scope
}

// matchResults pairs each configured repository with the create result borgmatic
// reported for it, by index into configured.
//
// The reported location is not always the configured string. borg normalizes
// what it is given (trailing slashes, relative paths, symlinks), and a path like
// ${BORG_REPO} is only resolved at borgmatic runtime, so the literal spelling
// the manager holds may never appear in the output at all. A missed pairing is
// silent: the run is still recorded as successful, but with no stats, so the
// repository's size, file count and duration simply never appear.
//
// Three passes, most trustworthy first:
//  1. exact location or label, which is the normal case
//  2. canonical path identity, which absorbs borg's normalization
//  3. one unmatched repository and one unclaimed result, which is unambiguous
//
// Positional pairing beyond that single-leftover case is deliberately not done.
// borgmatic emits results in configuration order today, but relying on it would
// mean silently attributing one destination's measurements to another whenever
// that stops holding, and wrong stats are worse than absent ones.
func matchResults(configured []config.RepoRef, results []createResult) map[int]createResult {
	out := make(map[int]createResult, len(configured))
	claimed := make([]bool, len(results))

	claim := func(refIdx, resIdx int) {
		out[refIdx] = results[resIdx]
		claimed[resIdx] = true
	}

	for i, ref := range configured {
		for j, res := range results {
			if claimed[j] {
				continue
			}
			if (ref.Path != "" && res.Repository.Location == ref.Path) ||
				(ref.Label != "" && res.Repository.Label == ref.Label) {
				claim(i, j)
				break
			}
		}
	}

	for i, ref := range configured {
		if _, done := out[i]; done || ref.Path == "" {
			continue
		}
		key := repoIdentity(ref.Path)
		if key == config.UnknownRepoKey {
			continue // resolved at borgmatic runtime; nothing to compare against
		}
		for j, res := range results {
			if claimed[j] || res.Repository.Location == "" {
				continue
			}
			if repoIdentity(res.Repository.Location) == key {
				claim(i, j)
				break
			}
		}
	}

	var loneRef, loneRes = -1, -1
	for i := range configured {
		if _, done := out[i]; done {
			continue
		}
		if loneRef >= 0 {
			return out // more than one candidate: no unambiguous pairing
		}
		loneRef = i
	}
	for j := range results {
		if claimed[j] {
			continue
		}
		if loneRes >= 0 {
			return out
		}
		loneRes = j
	}
	if loneRef >= 0 && loneRes >= 0 {
		claim(loneRef, loneRes)
	}
	return out
}

// applyStats copies a create result's archive stats onto a repo outcome.
// hasResult reports whether borgmatic measured this repository, keeping the
// probe skip and the outcome switch reading from one condition rather than two
// that could drift apart.
func hasResult(measured map[int]createResult, i int) bool {
	_, ok := measured[i]
	return ok
}

func applyStats(ro *state.RepoOutcome, res createResult) {
	ro.Measured = true
	ro.CompletedAt = parseBorgTime(res.Archive.End)
	ro.Files = res.Archive.Stats.NFiles
	ro.OriginalBytes = res.Archive.Stats.OriginalSize
	ro.CompressedBytes = res.Archive.Stats.CompressedSize
	ro.DeduplicatedBytes = res.Archive.Stats.DeduplicatedSize
	ro.DurationSeconds = res.Archive.Duration
}

// perRepoSuccess builds per-repository outcomes for a fully-successful group
// run: every configured repository backed up. Those with a create result carry
// its stats; a repo without one (a prune/check-only cycle) is still ok, sans stats.
func (r *Runner) perRepoSuccess(configured []config.RepoRef, results []createResult) []state.RepoOutcome {
	if len(configured) == 0 {
		return nil
	}
	// A maintenance-only cycle (prune/compact/check with no create) exits zero
	// without writing an archive anywhere. Recording that as a per-repository
	// success would advance every repository's last-success, reset its staleness
	// gauge and count an ok backup, so a manager configured that way would look
	// permanently healthy while never backing anything up.
	if !r.actionsInclude(actionCreate) {
		return nil
	}
	matched := matchResults(configured, results)
	out := make([]state.RepoOutcome, 0, len(configured))
	for i, ref := range configured {
		ro := state.RepoOutcome{ID: refID(ref), Path: resolvedRepoPath(ref), Result: state.ResultOK}
		if res, ok := matched[i]; ok {
			applyStats(&ro, res)
		}
		out = append(out, ro)
	}
	return out
}

// measuredOutcomes builds per-repository outcomes for a run that was stopped
// rather than one that failed: a timeout, or a signal during shutdown.
//
// An interrupted run confirms nothing, so nothing is probed and a repository
// borgmatic did not report is left out, keeping its stored state untouched. What
// it did report is another matter. create runs first, so a run killed while a
// later prune, compact or check hangs has already written archives and already
// said so, with their measurements. Discarding those leaves the per-repository
// last-success and sizes stale for a backup that demonstrably happened, and the
// group-level outcome keeps the same numbers anyway, so the two disagreed.
//
// The run as a whole stays terminated. This is about which destinations are
// known to hold a fresh archive, not about calling the run a success.
func measuredOutcomes(configured []config.RepoRef, results []createResult) []state.RepoOutcome {
	if len(configured) == 0 || len(results) == 0 {
		return nil
	}
	matched := matchResults(configured, results)
	out := make([]state.RepoOutcome, 0, len(matched))
	for i, ref := range configured {
		res, ok := matched[i]
		if !ok {
			continue
		}
		ro := state.RepoOutcome{ID: refID(ref), Path: resolvedRepoPath(ref), Result: state.ResultOK}
		applyStats(&ro, res)
		out = append(out, ro)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// perRepoFailure builds per-repository outcomes for a failed group run.
// borgmatic suppresses create --json entirely when any repository fails, so
// there is no positive per-repo signal in its output. It does, however, name
// the failing repository in an error record, and it soft-fails past a broken
// repo to run the rest to completion. So: a repo named in an error failed
// outright; an unimplicated repo is ambiguous (it may have succeeded, or the run
// may have aborted before any repo ran, e.g. a failing pre-backup hook) and is
// confirmed by probing for a fresh archive. A repo that can be neither implicated
// nor confirmed is left out (nil), so its persisted last-success is untouched
// rather than falsely advanced or failed.
func (r *Runner) perRepoFailure(ctx context.Context, configPath string, configured []config.RepoRef,
	results []createResult, scope archiveScope, run *runState, runStart time.Time,
) []state.RepoOutcome {
	if len(configured) == 0 {
		return nil
	}
	errText := run.errorMessages()

	if scope.anyAmbiguous() && len(results) == 0 {
		r.logger.Warn("cannot confirm which destinations this group reached: its archive pattern also matches another group's archives; rename a group or split repositories",
			"group", run.group, "pattern", scope.pattern)
	}

	// A create result is direct evidence: borgmatic reported the archive it
	// wrote, with its measurements. A run can still fail afterwards, when a
	// later action such as prune, compact or check exits nonzero, and rebuilding
	// every repository from confirmation probes then throws away measurements
	// that were in hand. The probe can only answer "an archive exists", so those
	// repositories were recorded ok with no stats at all, leaving the per-
	// repository size, file count and duration stale, or absent entirely if this
	// was the first backup.
	//
	// Evidence beats inference: a repository with a create result is not probed,
	// and not treated as implicated by an error naming it either. Error matching
	// is a substring test against the message text, and a prune failure names the
	// same path the successful create just reported.
	measured := matchResults(configured, results)

	// Resolved once per repository rather than per lookup: mentionedInErrors is
	// asked about every repository twice (to decide whether to probe, then to
	// build the outcome), and resolving a spelling can hit the filesystem.
	implicated := implicatedRepos(configured, errText)

	// Probed together rather than one after another. Every one of this group's
	// repository locks is still held here, and another group sharing one of them
	// is skipped for the whole time: serially that is probeTimeout per
	// repository, so a group with several unreachable destinations could hold
	// them for minutes. Concurrently the worst case is one probeTimeout no
	// matter how many there are, and each probe touches a different repository
	// so they do not contend.
	confirmed := make([]bool, len(configured))
	confirmedAt := make([]time.Time, len(configured))
	var wg sync.WaitGroup
	for i, ref := range configured {
		if _, ok := measured[i]; ok {
			continue // borgmatic already reported its archive; nothing to probe
		}
		if !scope.canConfirm(ref.Path) {
			// The pattern also matches a sibling group's archives, so a probe
			// answers a different question from the one being asked: a sibling
			// backup landing in the same whole second would confirm a success
			// this group never had. Refusing to confirm leaves the repository
			// untouched, which is wrong in a way that shows up as a stale
			// last-success rather than as a false fresh one.
			continue
		}
		if ref.Path == "" || implicated[i] {
			continue // named in an error: it failed, and probing it is pointless
		}
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			confirmed[i], confirmedAt[i] = r.confirmRepoSucceeded(ctx, configPath, path, scope.pattern, runStart)
		}(i, ref.Path)
	}
	wg.Wait()

	out := make([]state.RepoOutcome, 0, len(configured))
	for i, ref := range configured {
		ro := state.RepoOutcome{ID: refID(ref), Path: resolvedRepoPath(ref), Result: state.ResultFailed}
		switch {
		case hasResult(measured, i):
			ro.Result = state.ResultOK
			applyStats(&ro, measured[i])
		case implicated[i]:
			// This destination is named in an error: it failed.
		case confirmed[i]:
			ro.Result = state.ResultOK
			// The probe found the archive and knows when it was written. Without
			// carrying that, a destination whose archive completed early is
			// stamped with the end of a run that failed hours later, reporting an
			// old archive as fresh and delaying its staleness alert by the
			// difference.
			ro.CompletedAt = confirmedAt[i]
		default:
			// Neither implicated nor confirmed (probe skipped on shutdown, or
			// no fresh archive): leave this repo's stored state untouched.
			continue
		}
		out = append(out, ro)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mentionedInErrors reports whether repoPath appears as a path token in any
// error message, boundary-checked so "/data" does not match "/data2" or
// "/srv/data".
// implicatedRepos decides, for every configured repository at once, which ones
// the error messages name.
//
// Deciding each repository independently is what made "/mnt/repo" implicated by
// a message about "/mnt/repo old": the space ends the shorter path's token, so
// its match looked genuine. At a given position in a message only one
// repository can be the one named, and it is the longest one that matches
// there, so the decision has to be made across the set rather than one path at
// a time.
//
// The healthy destination is not merely mislabelled when this goes wrong: an
// implicated repository is never probed, so its successful archive goes
// unconfirmed too.
func implicatedRepos(configured []config.RepoRef, errText []string) []bool {
	out := make([]bool, len(configured))

	// Every spelling of every repository, longest first, so the first match at a
	// position is the most specific one.
	type candidate struct {
		index    int
		spelling string
	}
	var candidates []candidate
	for i, ref := range configured {
		for _, spelling := range repoSpellings(ref.Path) {
			candidates = append(candidates, candidate{index: i, spelling: spelling})
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return len(candidates[a].spelling) > len(candidates[b].spelling)
	})

	for _, msg := range errText {
		claimed := make([]bool, len(msg)+1)
		for _, cand := range candidates {
			for _, start := range tokenOccurrences(msg, cand.spelling) {
				if claimed[start] {
					continue // a longer repository already owns this position
				}
				claimed[start] = true
				out[cand.index] = true
			}
		}
	}
	return out
}

// tokenOccurrences lists the offsets where path appears in msg as a whole token.
func tokenOccurrences(msg, path string) []int {
	if path == "" {
		return nil
	}
	var out []int
	for idx := 0; ; {
		i := strings.Index(msg[idx:], path)
		if i < 0 {
			return out
		}
		start := idx + i
		end := start + len(path)
		beforeOK := start == 0 || !isNameByte(msg[start-1])
		afterOK := end >= len(msg) || !isNameByte(msg[end])
		if beforeOK && afterOK {
			out = append(out, start)
		}
		idx = start + 1
	}
}

// mentionedInErrors reports whether any error message names this repository.
//
// borg reports the location it resolved, not the string the config held, so the
// literal spelling is only one of the ways the same repository can appear: a
// configured "/mnt/repo/" is reported as "/mnt/repo", a relative or symlinked
// path arrives resolved. matchResults already canonicalizes on the success side;
// leaving this side literal meant a destination that borg named as failed went
// unrecognized, was dropped from persisted state entirely, and in a mixed
// fan-out was counted as unknown rather than failed: the one outcome an operator
// most needs to see, reported as the one thing it definitely was not.
func mentionedInErrors(repoPath string, errText []string) bool {
	for _, spelling := range repoSpellings(repoPath) {
		for _, msg := range errText {
			if containsPathToken(msg, spelling) {
				return true
			}
		}
	}
	return false
}

// repoSpellings lists the forms a repository path may take in borg's output:
// as configured, with a trailing separator trimmed, and canonicalized. Duplicates
// and the unresolvable key are dropped, so a runtime-expanded path contributes
// only its literal form rather than matching every other unresolvable path.
func repoSpellings(repoPath string) []string {
	if repoPath == "" {
		return nil
	}
	out := []string{repoPath}
	add := func(s string) {
		if s == "" || s == config.UnknownRepoKey {
			return
		}
		for _, seen := range out {
			if seen == s {
				return
			}
		}
		out = append(out, s)
	}
	add(strings.TrimRight(repoPath, "/"))
	add(config.CanonicalRepoKey(repoPath))

	// borgmatic resolves ${BORG_REPO} and friends from the environment this
	// process hands it, so the same expansion here recovers the location borg
	// will name in an error. Without it the literal spelling is the only one on
	// the list, an implicated destination goes unrecognized, and a repository
	// borg said outright had failed is reported as unknown.
	//
	// CanonicalRepoKey cannot do this itself: it answers "which repositories
	// must not run concurrently", and there "unknown" has to keep colliding with
	// every other unresolvable path. Expanding is the right answer for matching
	// and the wrong one for locking.
	if expanded := os.ExpandEnv(repoPath); expanded != repoPath {
		add(expanded)
		add(strings.TrimRight(expanded, "/"))
		add(repoIdentity(repoPath))
	}
	return out
}

// containsPathToken reports whether path occurs in msg as a whole path, not as a
// fragment of a longer name. A name-continuation byte (alphanumeric, '.', '-',
// '_') on either side means the match is a fragment ("/data" inside "/data2")
// and is rejected. A path separator ('/', ':') after the match is a valid
// boundary, so a repo path is still matched when the message names a child of it
// (a lock file "<repo>/lock.exclusive" or an archive "<repo>::name").
func containsPathToken(msg, path string) bool {
	for idx := 0; ; {
		i := strings.Index(msg[idx:], path)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(path)
		beforeOK := start == 0 || !isNameByte(msg[start-1])
		afterOK := end >= len(msg) || !isNameByte(msg[end])
		if beforeOK && afterOK {
			return true
		}
		idx = start + 1
	}
}

// isNameByte reports whether b can continue a single path-name component. '/'
// and ':' are excluded: they separate components, so they end a path token.
// isNameByte reports whether a byte continues a path token rather than ending
// it, which is what stops one repository's path matching inside another's.
//
// The set is deliberately wider than alphanumerics: a directory name may legally
// contain @, +, =, ~, %, # and more. With "/mnt/repo" and "/mnt/repo@old"
// configured, treating @ as a delimiter made an error naming only the second one
// implicate the first, so a destination that had backed up was recorded as
// failed alongside the one that really had.
//
// What is left out is what a message uses to separate a path from prose:
// whitespace, quotes, brackets, and the punctuation that ends a clause. The
// colon stays a delimiter because "Repository /mnt/repo: does not exist" is a
// shape borg actually emits, and reading it as part of the path would stop the
// repository being recognized at all. That costs a path that genuinely ends in a
// colon, which is the rarer case by a wide margin.
func isNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '-', b == '_':
		return true
	case b == '@', b == '+', b == '=', b == '~', b == '%', b == '#', b == '^', b == '!':
		return true
	case b >= 0x80:
		// A UTF-8 continuation or lead byte: part of a filename, never a
		// delimiter a log message writes. Without this, "/mnt/repo" matched
		// inside "/mnt/repoé" and a healthy destination was recorded as failed
		// on the strength of an error about a different one.
		return true
	default:
		return false
	}
}

// borgmaticListAction is borgmatic's archive-listing action, used by the
// per-repository confirmation probe.
const borgmaticListAction = "list"

// probeTimeout bounds a single per-repository confirmation list: an unreachable
// destination must not stall the failure path.
const probeTimeout = 60 * time.Second

// confirmRepoSucceeded reports whether repoPath holds an archive created at or
// after runStart, the definitive per-repository success signal when create
// --json is suppressed by a group failure. Any uncertainty (shutdown in
// progress, unreachable repo, probe error, no fresh archive) returns false: the
// caller then leaves the repo's stored state untouched rather than crediting it.
func (r *Runner) confirmRepoSucceeded(ctx context.Context, configPath, repoPath, archivePattern string, runStart time.Time) (bool, time.Time) {
	if repoPath == "" || ctx.Err() != nil {
		return false, time.Time{}
	}
	out, ok := r.runProbe(ctx, configPath, repoPath, archivePattern)
	if !ok {
		return false, time.Time{}
	}
	return newestArchiveFromThisRun(out, runStart)
}

// runProbe runs 'borgmatic list --repository <path> --last 1 --json', enforcing
// probeTimeout itself (the exec seam does not bind ctx), and returns the raw
// stdout. ok is false on any start/timeout/exit error.
func (r *Runner) runProbe(ctx context.Context, configPath, repoPath, archivePattern string) ([]byte, bool) {
	args := []string{"--config", configPath, borgmaticListAction, "--repository", repoPath, "--json", "--last", "1"}
	if archivePattern != "" {
		// Scoped to this group's own archives. Groups may share a repository,
		// so the newest archive in it can belong to someone else: without this
		// another group's fresh archive confirms this group's failed run as a
		// success, advancing its last-success and silencing the alert.
		args = append(args, "--match-archives", archivePattern)
	}
	cmd := r.execCommand(ctx, r.borgmaticPath, args...)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		r.logger.Warn("cannot start per-repository confirmation probe; treating repo as failed",
			"repo", repoPath, "error", err)
		return nil, false
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()

	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case <-exited:
	case <-ctx.Done():
		r.terminateGroup(cmd, exited, "probe:"+repoPath)
		<-exited
		return nil, false
	case <-timer.C:
		r.logger.Warn("per-repository confirmation probe timed out; treating repo as failed",
			"repo", repoPath, "timeout", probeTimeout)
		r.terminateGroup(cmd, exited, "probe:"+repoPath)
		<-exited
		return nil, false
	}
	if waitErr != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// listResult mirrors one repository entry of borgmatic list --json.
type listResult struct {
	Archives []struct {
		Start string `json:"start"`
		Time  string `json:"time"`
	} `json:"archives"`
}

// borgTimeLayouts are the local-time formats borg emits in list --json
// (microseconds optional).
var borgTimeLayouts = []string{borgTimeLayoutMicros, "2006-01-02T15:04:05"}

// borgTimeLayoutMicros is the sub-second form. Which layout parsed is what tells
// the comparison whether it can trust the archive's second.
const borgTimeLayoutMicros = "2006-01-02T15:04:05.000000"

// archiveIsFromThisRun decides whether an archive timestamp belongs to this run.
//
// With sub-second precision the comparison is exact, and there is nothing to
// decide: borg said when the archive started, and this run started before or
// after it.
//
// With whole-second precision the archive's own second is ambiguous. An archive
// stamped 12:00:00 against a run that started at 12:00:00.500 may have been
// written at 12:00:00.100, before the run, or at 12:00:00.900, by it. Truncating
// the run start resolves that in favour of confirming, which credits a failed
// run with an archive a previous run wrote: the exact case of a small backup
// succeeding and an immediate retry failing inside one second.
//
// So the ambiguous second is refused instead. That can leave a repository
// unjudged when this run's own archive lands in the second the run began, and
// the cost of being wrong that way is a last-success that stops advancing until
// the next run confirms it. The cost of being wrong the other way is a
// destination reported as freshly backed up when it was not, which is the alert
// that exists to fire. Refusing is the direction that fails loudly.
func archiveIsFromThisRun(archive time.Time, precise bool, runStart time.Time) bool {
	if precise {
		return !archive.Before(runStart)
	}
	return archive.Truncate(time.Second).After(runStart.Truncate(time.Second))
}

// newestArchiveAtOrAfter reports whether any archive in the list result was
// written by this run rather than a previous one.
func newestArchiveAtOrAfter(raw []byte, runStart time.Time) bool {
	ok, _ := newestArchiveFromThisRun(raw, runStart)
	return ok
}

// newestArchiveFromThisRun also reports when that archive was written, so a
// destination confirmed by probe is dated from its archive rather than from the
// end of a run that may have failed hours later.
func newestArchiveFromThisRun(raw []byte, runStart time.Time) (bool, time.Time) {
	var results []listResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return false, time.Time{}
	}
	var newest time.Time
	found := false
	for _, res := range results {
		for _, a := range res.Archives {
			ts := a.Start
			if ts == "" {
				ts = a.Time
			}
			for _, layout := range borgTimeLayouts {
				t, err := time.ParseInLocation(layout, ts, time.Local)
				if err != nil {
					continue
				}
				if archiveIsFromThisRun(t, layout == borgTimeLayoutMicros, runStart) {
					found = true
					if t.After(newest) {
						newest = t
					}
				}
				break
			}
		}
	}
	return found, newest
}

// parseBorgTime reads one of borg's local-time stamps, returning the zero time
// when it is absent or unparsable so callers fall back rather than guess.
func parseBorgTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	for _, layout := range borgTimeLayouts {
		if t, err := time.ParseInLocation(layout, ts, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// createResult mirrors one repository entry of create --json output. The first
// entry is representative for group-level dataset size; the full list drives
// per-repository outcomes.
type createResult struct {
	Archive struct {
		Name string `json:"name"`
		// End is when borg finished writing this archive. borgmatic reports it in
		// the same local-time layouts as list --json.
		End      string  `json:"end"`
		Duration float64 `json:"duration"`
		Stats    struct {
			NFiles           int64 `json:"nfiles"`
			OriginalSize     int64 `json:"original_size"`
			CompressedSize   int64 `json:"compressed_size"`
			DeduplicatedSize int64 `json:"deduplicated_size"`
		} `json:"stats"`
	} `json:"archive"`
	Repository struct {
		Location string `json:"location"`
		Label    string `json:"label"`
	} `json:"repository"`
}

// parseCreateResults stream-decodes buffered stdout (the result can arrive
// concatenated with log records) and returns every repository's create result.
func (rs *runState) parseCreateResults() []createResult {
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
			return results
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

// maxErrText bounds how many error bodies are kept for repo attribution; a
// failing run names its repo early, so a modest cap suffices under log spam.
const maxErrText = 500

// maxErrTextBytes bounds the total error text retained alongside the entry
// count. Entries are capped in number but not in size, and the scanner accepts
// lines up to a megabyte, so a pathologically noisy run could hold hundreds of
// megabytes for its whole duration. Path matching only needs enough of each
// message to find a repository path in it.
const maxErrTextBytes = 256 << 10

// recordErrorText keeps the full (untruncated) error body: a repo path can sit
// past the reason-truncation point, so path matching needs the whole message.
func (rs *runState) recordErrorText(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	rs.errTextMu.Lock()
	defer rs.errTextMu.Unlock()
	if len(rs.errText) >= maxErrText || rs.errTextBytes >= maxErrTextBytes {
		return
	}
	rs.errText = append(rs.errText, msg)
	rs.errTextBytes += len(msg)
}

func (rs *runState) errorMessages() []string {
	rs.errTextMu.Lock()
	defer rs.errTextMu.Unlock()
	out := make([]string, len(rs.errText))
	copy(out, rs.errText)
	return out
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
			rs.recordErrorText(rec.Message)
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
