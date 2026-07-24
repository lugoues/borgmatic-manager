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
	Running *statusRunning    `json:"running,omitempty"`
	Refused string            `json:"refused,omitempty"`
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

func buildStatusDoc(bs *models.BackupState, store *state.ScheduleStore, period, runTimeout time.Duration, filePeriods map[string]time.Duration, refused map[string]string, now time.Time) statusDoc {
	running := map[string]time.Time{}
	for _, p := range store.PendingSnapshot() {
		if started, ok := running[p.Group]; !ok || p.Started.Before(started) {
			running[p.Group] = p.Started
		}
	}

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

		rec, ok := store.Record(name)
		if ok && rec.LastRun != nil {
			lr := *rec.LastRun
			lr.LogTail = nil // logs belong to inspect; keep status output bounded
			g.LastRun = &lr
		}

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

func printStatusJSON(bs *models.BackupState, store *state.ScheduleStore, period, runTimeout time.Duration, filePeriods map[string]time.Duration, refused map[string]string) error {
	doc := buildStatusDoc(bs, store, period, runTimeout, filePeriods, refused, time.Now())
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding status: %w", err)
	}
	fmt.Println(string(out))
	return nil
}
