package metrics

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

type fakeSource struct{ snap map[string]state.GroupRecord }

func (f fakeSource) Snapshot() map[string]state.GroupRecord { return f.snap }

func newTestEmitter(t *testing.T, src StateSource) (*Emitter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, err := newEmitter(context.Background(), reader, "test", src, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	return e, reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Metrics{}
}

func attr(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

func TestRecordRunCountsPerRepository(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{})
	e.RecordRun("web", state.RunOutcome{
		Result: state.ResultFailed,
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultFailed},
		},
	})

	sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		got[attr(dp.Attributes, "repository")+"/"+attr(dp.Attributes, "result")] = dp.Value
	}
	assert.Equal(t, int64(1), got["local/ok"], "the healthy destination counts one ok run")
	assert.Equal(t, int64(1), got["offsite/failed"], "the down destination counts one failed run")
	assert.Equal(t, "web", attr(sum.DataPoints[0].Attributes, "group"))
}

func TestObservableGaugesReadFromState(t *testing.T) {
	now := time.Now()
	src := fakeSource{snap: map[string]state.GroupRecord{
		"web": {Repositories: map[string]state.RepoRecord{
			"local": {
				LastSuccess: now.Add(-90 * time.Second),
				LastRun: &state.RepoOutcome{
					Result: state.ResultOK, Files: 1234,
					OriginalBytes: 5000, CompressedBytes: 3000, DeduplicatedBytes: 800,
					DurationSeconds: 42,
				},
			},
			"offsite": { // failed and never succeeded: no staleness sample
				LastRun: &state.RepoOutcome{Result: state.ResultFailed},
			},
		}},
	}}
	e, reader := newTestEmitter(t, src)
	e.now = func() time.Time { return now }
	rm := collect(t, reader)

	// Sizes: three kinds for the local repo.
	sizes := findMetric(t, rm, "backup_last_size_bytes").Data.(metricdata.Gauge[int64])
	byKind := map[string]int64{}
	for _, dp := range sizes.DataPoints {
		if attr(dp.Attributes, "repository") == "local" {
			byKind[attr(dp.Attributes, "kind")] = dp.Value
		}
	}
	assert.Equal(t, int64(5000), byKind["original"])
	assert.Equal(t, int64(3000), byKind["compressed"])
	assert.Equal(t, int64(800), byKind["deduplicated"])

	files := findMetric(t, rm, "backup_last_files").Data.(metricdata.Gauge[int64])
	assert.Equal(t, int64(1234), files.DataPoints[0].Value)

	dur := findMetric(t, rm, "backup_last_duration_seconds").Data.(metricdata.Gauge[float64])
	assert.InDelta(t, 42.0, dur.DataPoints[0].Value, 0.001)

	// Staleness: reported for the repo that has succeeded, not for the one that
	// never has.
	stale := findMetric(t, rm, "backup_seconds_since_last_success").Data.(metricdata.Gauge[float64])
	var repos []string
	for _, dp := range stale.DataPoints {
		repos = append(repos, attr(dp.Attributes, "repository"))
		if attr(dp.Attributes, "repository") == "local" {
			assert.InDelta(t, 90.0, dp.Value, 1.0)
		}
	}
	assert.Equal(t, []string{"local"}, repos, "a never-succeeded repo has no staleness value")
}

func TestOfflineVolumesGauge(t *testing.T) {
	bs := models.NewBackupState()
	bs.AddVolume("web", models.VolumeInfo{Name: "up", HostPath: "/mnt/up"})
	bs.AddVolume("web", models.VolumeInfo{Name: "down", HostPath: "/mnt/down"})

	off := &state.Offline{Volumes: map[string]map[string]bool{"web": {"down": true}}}
	e, reader := newTestEmitter(t, fakeSource{})
	e.ObserveInventory(bs, off)

	g := findMetric(t, collect(t, reader), "backup_offline_volumes").Data.(metricdata.Gauge[int64])
	require.Len(t, g.DataPoints, 1)
	assert.Equal(t, "web", attr(g.DataPoints[0].Attributes, "group"))
	assert.Equal(t, int64(1), g.DataPoints[0].Value, "one of two volumes is offline")
}

func TestNewExporterProtocol(t *testing.T) {
	ctx := context.Background()
	for _, proto := range []string{"", "http", "grpc", "GRPC"} {
		exp, err := newExporter(ctx, config.MetricsSettings{Protocol: proto, Endpoint: "http://localhost:4318"})
		require.NoError(t, err, "protocol %q should construct", proto)
		_ = exp.Shutdown(ctx)
	}
	_, err := newExporter(ctx, config.MetricsSettings{Protocol: "carrier-pigeon"})
	require.Error(t, err, "an unknown protocol is rejected")
}

// Every other series here appears only after a run, so without this a group
// that has never succeeded is indistinguishable from one that was deleted, and
// an alert on "no recent backup" cannot fire for the group that most needs it.
func TestGroupInfoIsEmittedForAGroupThatHasNeverRun(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{snap: map[string]state.GroupRecord{}})

	// A configured group with no volumes and no runs: the shape a freshly
	// labelled service has before its first backup.
	bs := models.NewBackupState()
	bs.AddVolume("never-run", models.VolumeInfo{Name: "data", HostPath: "/mnt/data"})
	e.ObserveInventory(bs, nil)

	rm := collect(t, reader)
	m := findMetric(t, rm, "backup_group_info")
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "backup_group_info must be an int64 gauge")
	require.Len(t, gauge.DataPoints, 1, "one series per configured group, run or not")
	assert.Equal(t, int64(1), gauge.DataPoints[0].Value)
	assert.Equal(t, "never-run", attr(gauge.DataPoints[0].Attributes, "group"))
}
