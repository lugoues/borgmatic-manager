package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/scheduler"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// statusDoc is the machine-readable form of the status display, sharing its
// data sources and dueness rules so the two cannot disagree.
type statusDoc struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Groups      []statusGroupJSON `json:"groups"`
}

type statusGroupJSON struct {
	Name          string `json:"name"`
	Volumes       int    `json:"volumes"`
	Databases     int    `json:"databases"`
	PeriodSeconds int64  `json:"period_seconds"`
	// LastRun is the persisted most-recent outcome (log tail stripped).
	LastRun *state.RunOutcome `json:"last_run,omitempty"`
	// Repositories is the per-destination breakdown for a fan-out group: each
	// repo's last outcome and its own last_success, which advances independently
	// so a healthy destination stays fresh while a sibling fails.
	Repositories map[string]state.RepoRecord `json:"repositories,omitempty"`
	Running      *statusRunning              `json:"running,omitempty"`
	Refused      string                      `json:"refused,omitempty"`
	// Offline is true when the group has no live container; it is still backed
	// up (its volumes are data at rest) and keeps its normal schedule.
	Offline bool `json:"offline,omitempty"`
	// OfflineVolumes/OfflineDatabases name the members whose container is gone.
	OfflineVolumes   []string `json:"offline_volumes,omitempty"`
	OfflineDatabases []string `json:"offline_databases,omitempty"`
	// Due/NextRun are omitted while running or refused.
	Due     *bool      `json:"due,omitempty"`
	NextRun *time.Time `json:"next_run,omitempty"`
}

type statusRunning struct {
	Started        time.Time `json:"started"`
	ElapsedSeconds int64     `json:"elapsed_seconds"`
	// Stale marks a run past run_timeout: possibly a dead process's leftover.
	Stale bool `json:"stale"`
}

func buildStatusDoc(bs *models.BackupState, store *state.ScheduleStore, lockDir string, period, runTimeout time.Duration, filePeriods map[string]time.Duration, refused map[string]string, configured map[string]map[string]string, off *state.Offline, now time.Time) statusDoc {
	running := runningGroups(store, lockDir)

	names := make([]string, 0, len(bs.Groups))
	for name := range bs.Groups {
		names = append(names, name)
	}
	sort.Strings(names)

	doc := statusDoc{GeneratedAt: now, Groups: []statusGroupJSON{}}
	for _, name := range names {
		group := bs.Groups[name]
		if len(group.Volumes) == 0 && len(group.Databases) == 0 {
			continue
		}

		g := statusGroupJSON{
			Name:          name,
			Volumes:       len(group.Volumes),
			Databases:     len(group.Databases),
			PeriodSeconds: int64(scheduler.EffectivePeriod(group, filePeriods[name], period) / time.Second),
		}
		if off != nil {
			g.Offline = off.GroupOffline(name, group)
			for _, v := range group.Volumes {
				if off.VolumeOffline(name, v.Name) {
					g.OfflineVolumes = append(g.OfflineVolumes, v.Name)
				}
			}
			for _, db := range group.Databases {
				if off.DatabaseOffline(name, db) {
					g.OfflineDatabases = append(g.OfflineDatabases, db.Type+"/"+db.Name)
				}
			}
		}

		rec, ok := store.Record(name)
		if ok && rec.LastRun != nil {
			lr := *rec.LastRun
			lr.LogTail = nil // logs belong to inspect; keep status output bounded
			g.LastRun = &lr
		}
		if ok && len(rec.Repositories) > 0 {
			g.Repositories = currentRepositories(rec.Repositories, configured[name])
		}

		// Offline groups are still backed up, so they carry a normal schedule.
		switch started, isRunning := running[name]; {
		case isRunning:
			elapsed := now.Sub(started)
			g.Running = &statusRunning{
				Started:        started,
				ElapsedSeconds: int64(elapsed / time.Second),
				Stale:          runTimeout > 0 && elapsed > runTimeout,
			}
		case refused[name] != "":
			g.Refused = refused[name]
		default:
			due := scheduler.Due(rec, ok, scheduler.GroupFingerprint(group), scheduler.EffectivePeriod(group, filePeriods[name], period), now)
			g.Due = &due.Due
			if !due.Due {
				next := due.Next
				g.NextRun = &next
			}
		}

		doc.Groups = append(doc.Groups, g)
	}
	return doc
}

// currentRepositories drops persisted records for repositories the group no
// longer configures, or that now point somewhere else.
//
// The persisted map is history, not inventory. Repository settings do not enter
// the scheduler fingerprint, so removing a repository from a recently successful
// group leaves it not due, no run reconciles the record, and status would report
// the deleted destination for a whole period. A repointed label is the same
// problem wearing the same name.
//
// A group the caller knows nothing about is passed through: silence is not a
// report that it configures nothing.
func currentRepositories(persisted map[string]state.RepoRecord, configured map[string]string) map[string]state.RepoRecord {
	if configured == nil {
		return persisted
	}
	out := make(map[string]state.RepoRecord, len(persisted))
	for id, rr := range persisted {
		path, stillConfigured := configured[id]
		if !stillConfigured {
			continue
		}
		if path != "" && rr.Path != "" && rr.Path != path {
			continue // the id survived a repoint; this history is the old destination's
		}
		out[id] = rr
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func printStatusJSON(bs *models.BackupState, store *state.ScheduleStore, lockDir string, period, runTimeout time.Duration, filePeriods map[string]time.Duration, refused map[string]string, configured map[string]map[string]string, off *state.Offline) error {
	doc := buildStatusDoc(bs, store, lockDir, period, runTimeout, filePeriods, refused, configured, off, time.Now())
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding status: %w", err)
	}
	fmt.Println(string(out))
	return nil
}
