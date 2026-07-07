package state

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GroupRecord is one group's persisted schedule state.
type GroupRecord struct {
	// LastSuccess is when the last successful run started; anchoring to starts
	// keeps the period stable regardless of backup duration.
	LastSuccess time.Time `json:"last_success"`
	// Fingerprint identifies the backed-up content set; a membership change
	// makes the group due immediately.
	Fingerprint string `json:"fingerprint"`
}

type scheduleFile struct {
	Version int                    `json:"version"`
	Groups  map[string]GroupRecord `json:"groups"`
}

// ScheduleStore holds per-group schedule records, persisted as JSON in the
// state directory. Every failure mode (missing file, corrupt file, write
// error) degrades to "the group is due": an extra backup is recoverable,
// a silently skipped one is not.
type ScheduleStore struct {
	path   string
	logger *slog.Logger

	mu     sync.Mutex
	groups map[string]GroupRecord
}

// LoadSchedule reads the schedule state from stateDir, returning an empty
// (everything-due) store when the file is missing or unreadable.
func LoadSchedule(stateDir string, logger *slog.Logger) *ScheduleStore {
	s := &ScheduleStore{
		path:   filepath.Join(stateDir, "schedule.json"),
		logger: logger,
		groups: map[string]GroupRecord{},
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err != nil {
		logger.Warn("cannot read schedule state; treating all groups as due", "path", s.path, "error", err)
		return s
	}

	var f scheduleFile
	if err := json.Unmarshal(data, &f); err != nil {
		logger.Warn("schedule state is corrupt; treating all groups as due", "path", s.path, "error", err)
		return s
	}
	if f.Groups != nil {
		s.groups = f.Groups
	}
	return s
}

// Record returns the stored record for a group.
func (s *ScheduleStore) Record(name string) (GroupRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.groups[name]
	return rec, ok
}

// Snapshot returns a copy of all records, for schedule computations.
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
// schedule survives a crash between the run and daemon shutdown.
func (s *ScheduleStore) MarkSuccess(name, fingerprint string, startedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[name] = GroupRecord{LastSuccess: startedAt, Fingerprint: fingerprint}
	s.save()
}

// Retain drops records for groups that no longer exist, so stale entries
// can't distort next-wake computations. Persists only if something changed.
func (s *ScheduleStore) Retain(names map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := false
	for name := range s.groups {
		if _, ok := names[name]; !ok {
			delete(s.groups, name)
			pruned = true
		}
	}
	if pruned {
		s.save()
	}
}

// save writes atomically (tmp + rename). Callers hold s.mu. Persistence
// errors are logged, never propagated: failing a backup over schedule
// bookkeeping would invert the priorities.
func (s *ScheduleStore) save() {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		s.logger.Warn("cannot create state directory; schedule will not survive restarts", "path", s.path, "error", err)
		return
	}
	data, err := json.MarshalIndent(scheduleFile{Version: 1, Groups: s.groups}, "", "  ")
	if err != nil {
		s.logger.Warn("cannot encode schedule state", "error", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.logger.Warn("cannot write schedule state; schedule will not survive restarts", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.logger.Warn("cannot replace schedule state; schedule will not survive restarts", "path", s.path, "error", err)
	}
}
