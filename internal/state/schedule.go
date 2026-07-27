package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
)

// Result values for RunOutcome.Result.
const (
	ResultOK         = "ok"
	ResultFailed     = "failed"
	ResultTerminated = "terminated"
	// ResultUnknown labels an attempt that happened but could not be judged: the
	// run reached a repository and neither implicated nor confirmed it. It is a
	// metrics label only, never a stored outcome, because storing it would
	// overwrite what is actually known about that repository.
	ResultUnknown = "unknown"
)

// RunOutcome is the observed result of a group's most recent borgmatic
// run, successful or not, captured from the runner for status display.
type RunOutcome struct {
	Finished time.Time `json:"finished"`
	Result   string    `json:"result"` // ok | failed | terminated
	ExitCode int       `json:"exit_code"`
	// ConfiguredRepositories are the repository ids the group currently
	// configures, which is not the same as the ids in Repositories: a failed run
	// deliberately omits repositories it could neither implicate nor confirm, so
	// the outcome alone cannot say whether a missing one was skipped or removed.
	// Empty leaves the persisted set untouched.
	ConfiguredRepositories []string `json:"-"`
	// CreateAttempted records whether this run tried to write an archive at all.
	// A maintenance-only cycle (prune, compact, check, with no create) exits
	// zero having backed nothing up, and counting that as a successful backup
	// per destination would report a manager configured that way as permanently
	// healthy. Like ConfiguredRepositories this is an input to recording, not
	// persisted state.
	CreateAttempted bool `json:"-"`
	// LastError is the first CRITICAL/ERROR message from a failed run, so
	// status can show why without the journal.
	LastError       string `json:"last_error,omitempty"`
	Warnings        int64  `json:"warnings"`
	DurationSeconds int64  `json:"duration_seconds"`
	// LogTail is a bounded output tail kept only on the most recent run
	// (history drops it) to bound state size.
	LogTail []string `json:"log_tail,omitempty"`
	// Archive is the archive name borg reported creating, when observed.
	Archive string `json:"archive,omitempty"`
	// Create stats from borgmatic's create --json result (zero when the
	// run had no create action or no result was parsed). Group-level fields
	// are the first repository's, representative of the dataset size.
	Files             int64 `json:"files,omitempty"`
	OriginalBytes     int64 `json:"original_bytes,omitempty"`
	CompressedBytes   int64 `json:"compressed_bytes,omitempty"`
	DeduplicatedBytes int64 `json:"deduplicated_bytes,omitempty"`
	// Repositories carries the per-repository breakdown of this run: a
	// fan-out to several repos can have one fail while the others succeed.
	Repositories []RepoOutcome `json:"repositories,omitempty"`
}

// RepoOutcome is one repository's result within a run. ID is borgmatic's
// repository label when set, else the configured path.
type RepoOutcome struct {
	ID                string `json:"id"`
	Path              string `json:"path,omitempty"`
	Result            string `json:"result"` // ok | failed | terminated
	Files             int64  `json:"files,omitempty"`
	OriginalBytes     int64  `json:"original_bytes,omitempty"`
	CompressedBytes   int64  `json:"compressed_bytes,omitempty"`
	DeduplicatedBytes int64  `json:"deduplicated_bytes,omitempty"`
	DurationSeconds   int64  `json:"duration_seconds,omitempty"`
	// CompletedAt is when borg finished writing this archive, which is not when
	// the run finished. A create that succeeds and a check that then hangs until
	// a 24-hour timeout would otherwise stamp the repository as fresh at the
	// moment of the timeout, understating the archive's age by a day and
	// delaying the staleness alert by exactly as long as the hang lasted.
	CompletedAt time.Time `json:"completed_at,omitempty"`
	// Measured records that borgmatic reported this archive, as opposed to the
	// outcome being inferred from a confirmation probe. It is not the same
	// question as "are any of the numbers above nonzero": an empty archive that
	// finishes in under a second is legitimately all zeros, and inferring
	// measurement from nonzero values leaves such a backup reported under an
	// older, larger archive forever.
	Measured bool `json:"measured,omitempty"`
}

// RepoRecord persists a repository's last outcome and last success across runs,
// so per-repository staleness survives a run where that repo failed but others
// did not (the group-level LastSuccess would not advance in that case).
type RepoRecord struct {
	// Path is the destination this record's history belongs to. A labelled
	// repository keeps its key when it is repointed, so without this the new
	// destination inherits the old one's last success and reports as recently
	// backed up having never produced an archive.
	Path        string       `json:"path,omitempty"`
	LastSuccess time.Time    `json:"last_success"`
	LastRun     *RepoOutcome `json:"last_run,omitempty"`
	// LastStats is the most recent outcome that actually carried archive
	// statistics, kept separately from LastRun.
	//
	// Two outcomes are successes without stats: a later failure replaces
	// LastRun entirely, and a repository confirmed by the fallback probe is
	// recorded ok with nothing measured. Reading sizes from LastRun therefore
	// either stopped reporting them or reported zeros, and a dataset that
	// appears to shrink to nothing is worse than one that stops updating.
	LastStats *RepoOutcome `json:"last_stats,omitempty"`
}

// hasStats reports whether an outcome carries archive measurements. A
// probe-confirmed success has none.
// measuredStats reports whether this outcome carries archive statistics worth
// preserving. Measured is authoritative; the nonzero check is the fallback for
// an outcome written before that field existed, so upgrading does not lose the
// stats already on disk.
func (r RepoOutcome) measuredStats() bool {
	return r.Measured || r.hasStats()
}

func (r RepoOutcome) hasStats() bool {
	return r.Files > 0 || r.OriginalBytes > 0 || r.CompressedBytes > 0 ||
		r.DeduplicatedBytes > 0 || r.DurationSeconds > 0
}

// GroupRecord is one group's persisted schedule state.
type GroupRecord struct {
	// LastSuccess is when the last successful run started; anchoring to starts
	// keeps the period stable regardless of backup duration.
	LastSuccess time.Time `json:"last_success"`
	// Fingerprint identifies the backed-up content set; a membership change
	// makes the group due immediately.
	Fingerprint string `json:"fingerprint"`
	// LastRun is the most recent run's outcome, including failures,
	// scheduling only trusts LastSuccess, but status wants the truth.
	LastRun *RunOutcome `json:"last_run,omitempty"`
	// History is a bounded, oldest-first ring of past outcomes (log tails
	// stripped) for inspect. Capped at maxHistory.
	History []RunOutcome `json:"history,omitempty"`
	// Repositories holds per-repository last outcome and last success, keyed
	// by repository ID (label or path), for fan-out monitoring.
	Repositories map[string]RepoRecord `json:"repositories,omitempty"`
	// MissingCycles counts consecutive absent cycles; a record survives two
	// before pruning so a redeploy blip doesn't wipe schedules.
	MissingCycles int `json:"missing_cycles,omitempty"`
}

// PendingRun records a spawned run whose dump helpers may still exist: written
// before spawn, cleared after reap, so a crash leaves evidence to reap orphans.
type PendingRun struct {
	Group   string    `json:"group"`
	Started time.Time `json:"started"`
	// PID owns this run: reconciliation reaps a pending record only when its
	// owner is gone (ad-hoc runs share this file). Zero means orphan.
	PID int `json:"pid,omitempty"`
}

type scheduleFile struct {
	Version int                    `json:"version"`
	Groups  map[string]GroupRecord `json:"groups"`
	Pending map[string]PendingRun  `json:"pending_runs,omitempty"`
}

// errCorruptState marks an unparseable state file: a writer may overwrite
// corrupt garbage but never state it merely failed to read.
var errCorruptState = errors.New("schedule state is corrupt")

// ScheduleStore persists per-group schedule records as JSON. Every failure mode
// degrades to "everything due": an extra backup is recoverable, a skipped one is
// not. Cross-process safety: every mutation re-reads the shared file under an
// exclusive flock before writing back, so processes cannot erase each other.
type ScheduleStore struct {
	path     string
	lockPath string
	logger   *slog.Logger

	mu      sync.Mutex
	groups  map[string]GroupRecord
	pending map[string]PendingRun
}

// LoadSchedule reads the schedule state from stateDir, returning an empty
// (everything-due) store when the file is missing or unreadable.
func LoadSchedule(stateDir string, logger *slog.Logger) *ScheduleStore {
	s := &ScheduleStore{
		path:     filepath.Join(stateDir, "schedule.json"),
		lockPath: filepath.Join(stateDir, "schedule.json.lock"),
		logger:   logger,
		groups:   map[string]GroupRecord{},
		pending:  map[string]PendingRun{},
	}
	s.sweepStaleTempFiles()
	s.Reload()
	return s
}

// sweepStaleTempFiles removes temp files orphaned by a crash mid-write. It
// runs under the state flock, so any temp seen here cannot be a live write.
func (s *ScheduleStore) sweepStaleTempFiles() {
	lock, err := lockfile.Exclusive(s.lockPath)
	if err != nil {
		return // best-effort cleanup; not worth blocking startup
	}
	defer lock.Release()

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), "schedule-*.json.tmp"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("cannot remove orphaned schedule temp file", "path", m, "error", err)
		}
	}
}

// Reload refreshes the in-memory view to pick up other processes' writes. A
// read failure degrades to empty ("everything due"); writers do not use this path.
func (s *ScheduleStore) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.readFile()
	if err != nil {
		s.logger.Warn("cannot read schedule state; treating all groups as due", "path", s.path, "error", err)
	}
	s.groups, s.pending = f.Groups, f.Pending
}

// readFile parses the state file; missing means empty state, any other failure
// is returned so a writer never renames an empty file over good state. Reads
// need no lock: atomic rename means a reader sees whole or none.
func (s *ScheduleStore) readFile() (scheduleFile, error) {
	f := scheduleFile{Version: 1, Groups: map[string]GroupRecord{}, Pending: map[string]PendingRun{}}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}

	var parsed scheduleFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return f, fmt.Errorf("%w: %v", errCorruptState, err)
	}
	if parsed.Groups != nil {
		f.Groups = parsed.Groups
	}
	if parsed.Pending != nil {
		f.Pending = parsed.Pending
	}
	return f, nil
}

// update re-reads the file under an exclusive flock, applies mutate (which
// reports whether it changed anything), and writes back. A read failure aborts:
// an empty read must never clobber good state.
func (s *ScheduleStore) update(mutate func(*scheduleFile) (changed bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A lock we cannot take is not worth failing a backup over: warn and
	// proceed, which is no worse than the single-process behavior.
	if lock, err := lockfile.Exclusive(s.lockPath); err != nil {
		s.logger.Warn("cannot lock schedule state; proceeding unlocked", "path", s.lockPath, "error", err)
	} else {
		defer lock.Release()
	}

	f, err := s.readFile()
	corrupt := false
	switch {
	case errors.Is(err, errCorruptState):
		// Unparseable garbage: nothing to preserve, overwrite fresh.
		s.logger.Warn("schedule state is corrupt; rewriting it fresh", "path", s.path, "error", err)
		corrupt = true
	case err != nil:
		// Transient failure: disk state may be intact, must not clobber it.
		s.logger.Error("cannot read schedule state; skipping this update to avoid overwriting it", "path", s.path, "error", err)
		return
	}
	// When healing a corrupt file, write even on a no-op mutation, or the corpse
	// is never replaced and the warning repeats every cycle.
	if !mutate(&f) && !corrupt {
		return
	}
	s.writeFile(f)
	s.groups, s.pending = f.Groups, f.Pending
}

// RecordPending persists an in-flight run keyed by run ID, stamped with the
// owning PID so reconciliation can tell an orphan from a live run.
func (s *ScheduleStore) RecordPending(runID, group string, started time.Time) {
	s.update(func(f *scheduleFile) bool {
		f.Pending[runID] = PendingRun{Group: group, Started: started, PID: os.Getpid()}
		return true
	})
}

// ClearPending removes a pending-run record after its helpers are reaped.
func (s *ScheduleStore) ClearPending(runID string) {
	s.update(func(f *scheduleFile) bool {
		if _, ok := f.Pending[runID]; !ok {
			return false
		}
		delete(f.Pending, runID)
		return true
	})
}

// PendingSnapshot returns a copy of the pending-run records.
func (s *ScheduleStore) PendingSnapshot() map[string]PendingRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]PendingRun, len(s.pending))
	for id, p := range s.pending {
		out[id] = p
	}
	return out
}

// Record returns the stored record for a group.
func (s *ScheduleStore) Record(name string) (GroupRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.groups[name]
	return rec, ok
}

// Snapshot returns a copy of all records, for schedule computations.
// Snapshot returns the current per-group records.
//
// The returned GroupRecords share their Repositories map and History slice with
// the stored records, which is safe because those are never mutated after
// publication: every write goes through update, which parses a fresh copy from
// disk, mutates that, and swaps it in wholesale. A previously returned map is
// orphaned, not modified. TestSnapshotsAreNotMutatedByLaterWrites pins the
// invariant, because in-place mutation is the obvious optimization to make here
// and it would turn every snapshot into a concurrent map access.
func (s *ScheduleStore) Snapshot() map[string]GroupRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]GroupRecord, len(s.groups))
	for name, rec := range s.groups {
		out[name] = rec
	}
	return out
}

// MarkSuccess records a successful run and persists immediately, so the
// schedule survives a crash before shutdown.
func (s *ScheduleStore) MarkSuccess(name, fingerprint string, startedAt time.Time) {
	s.update(func(f *scheduleFile) bool {
		rec := f.Groups[name]
		rec.LastSuccess = startedAt
		rec.Fingerprint = fingerprint
		rec.MissingCycles = 0
		f.Groups[name] = rec
		return true
	})
}

// maxHistory bounds the per-group run history kept for `inspect`. Sized for a
// multi-week trend at hourly periods; entries are slim (log tails stripped),
// so state stays small.
const maxHistory = 90

// RecordRun stores a run outcome without touching schedule fields: the full
// outcome becomes LastRun, a log-stripped copy joins the bounded History.
func (s *ScheduleStore) RecordRun(name string, outcome RunOutcome) {
	s.update(func(f *scheduleFile) bool {
		rec := f.Groups[name]
		rec.LastRun = &outcome

		// Per-repository: advance each repo's last outcome, and its last
		// success independently, so a repo that succeeds keeps its freshness
		// even when a sibling repo fails and the group's LastSuccess stalls.
		for _, ro := range outcome.Repositories {
			if rec.Repositories == nil {
				rec.Repositories = map[string]RepoRecord{}
			}
			rr := rec.Repositories[ro.ID]
			// A record whose destination has changed is a record of something
			// else. Its history is dropped rather than inherited, so a repointed
			// label starts from nothing known rather than from the previous
			// destination's freshness. An empty stored path is a record written
			// before this field existed: adopted, not reset.
			if ro.Path != "" && rr.Path != "" && rr.Path != ro.Path {
				rr = RepoRecord{}
			}
			if ro.Path != "" {
				rr.Path = ro.Path
			}
			roCopy := ro
			rr.LastRun = &roCopy
			if ro.Result == ResultOK {
				// When the archive's own completion time is known, use it: the
				// run may have continued for hours afterwards on actions that
				// wrote nothing.
				rr.LastSuccess = outcome.Finished
				if !ro.CompletedAt.IsZero() {
					rr.LastSuccess = ro.CompletedAt
				}
				if ro.measuredStats() {
					statsCopy := ro
					rr.LastStats = &statsCopy
				}
			}
			rec.Repositories[ro.ID] = rr
		}

		// Reconcile the inventory against what the group configures now.
		//
		// Both directions matter. Repositories the group no longer configures are
		// dropped: left in place they linger forever in status, inspect and the
		// exported series, and runRepoHealth counts them in the total, so a
		// removed destination permanently reports the group as partial.
		//
		// Repositories it does configure are materialized even when this run
		// could not judge them. A destination added to an established group whose
		// next run fails early (a pre-backup hook, say) appears in neither the
		// outcome nor the record, so it is absent from the inventory series and
		// the documented alert join cannot see the one destination that has never
		// backed up. An empty record says "configured, nothing known yet", which
		// is exactly what wants alerting on.
		if len(outcome.ConfiguredRepositories) > 0 {
			current := make(map[string]bool, len(outcome.ConfiguredRepositories))
			for _, id := range outcome.ConfiguredRepositories {
				current[id] = true
				if rec.Repositories == nil {
					rec.Repositories = map[string]RepoRecord{}
				}
				if _, known := rec.Repositories[id]; !known {
					rec.Repositories[id] = RepoRecord{}
				}
			}
			for id := range rec.Repositories {
				if !current[id] {
					delete(rec.Repositories, id)
				}
			}
		}

		slim := outcome
		slim.LogTail = nil      // history keeps stats, not logs, only LastRun carries the tail
		slim.Repositories = nil // per-repo history lives in rec.Repositories, not per-outcome
		rec.History = append(rec.History, slim)
		if len(rec.History) > maxHistory {
			rec.History = rec.History[len(rec.History)-maxHistory:]
		}

		f.Groups[name] = rec
		return true
	})
}

// Retain reconciles records against this cycle's groups: a vanished group
// survives two absent cycles before pruning; presence resets the counter.
// Retain reconciles group records against the currently live groups. Active
// groups have their absence counter cleared. Absent groups age out (their
// record is deleted after two cycles) unless they are in protected: a
// protected-but-absent group keeps its record and last-backup forever, marked
// absent (MissingCycles > 0) so it does not drive NextWake. protected is the
// set of cached groups, so an offline group stays visible in status/discover.
func (s *ScheduleStore) Retain(active, protected map[string]struct{}) {
	s.update(func(f *scheduleFile) bool {
		changed := false
		for name, rec := range f.Groups {
			if _, ok := active[name]; ok {
				if rec.MissingCycles != 0 {
					rec.MissingCycles = 0
					f.Groups[name] = rec
					changed = true
				}
				continue
			}
			if _, keep := protected[name]; !keep && rec.MissingCycles >= 2 {
				delete(f.Groups, name)
			} else {
				rec.MissingCycles++
				f.Groups[name] = rec
			}
			changed = true
		}
		return changed
	})
}

// writeFile persists atomically via unique temp + rename (a fixed temp name
// would let two processes rename over each other); callers hold s.mu and the
// flock. Errors are logged, never propagated: bookkeeping must not fail a backup.
func (s *ScheduleStore) writeFile(f scheduleFile) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.logger.Warn("cannot create state directory; schedule will not survive restarts", "path", s.path, "error", err)
		return
	}
	f.Version = 1
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		s.logger.Warn("cannot encode schedule state", "error", err)
		return
	}

	tmp, err := os.CreateTemp(dir, "schedule-*.json.tmp")
	if err != nil {
		s.logger.Warn("cannot write schedule state; schedule will not survive restarts", "path", dir, "error", err)
		return
	}
	tmpName := tmp.Name()
	// A no-op once the rename succeeds; cleans up on every failure path.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		s.logger.Warn("cannot write schedule state; schedule will not survive restarts", "path", tmpName, "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		s.logger.Warn("cannot write schedule state; schedule will not survive restarts", "path", tmpName, "error", err)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		s.logger.Warn("cannot replace schedule state; schedule will not survive restarts", "path", s.path, "error", err)
	}
}
