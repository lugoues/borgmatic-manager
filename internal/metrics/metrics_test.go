package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
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
				// The measurements come from the last run that measured
				// anything, not the last run. LastRun here is a later failure
				// carrying zeros, which is what the gauges must not report.
				LastRun: &state.RepoOutcome{Result: state.ResultFailed},
				LastStats: &state.RepoOutcome{
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

// The OTLP spec makes these variables the standard way to point an application
// at a collector, and every other OTLP setting here is already read from the
// environment by the exporter itself. Ignoring the protocol meant a deployment
// that set OTEL_EXPORTER_OTLP_PROTOCOL=grpc got an HTTP exporter talking to a
// gRPC collector: no error, no metrics.
func TestProtocolFallsBackToTheStandardEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		env        map[string]string
		want       string
	}{
		{name: "config wins over both", configured: "grpc",
			env:  map[string]string{envMetricsProtocol: "http", envProtocol: "http"},
			want: "grpc"},
		{name: "metrics-specific wins over generic",
			env:  map[string]string{envMetricsProtocol: "grpc", envProtocol: "http/protobuf"},
			want: "grpc"},
		{name: "generic applies when metrics-specific is unset",
			env: map[string]string{envProtocol: "grpc"}, want: "grpc"},
		{name: "an empty metrics-specific value does not mask the generic one",
			env:  map[string]string{envMetricsProtocol: "  ", envProtocol: "grpc"},
			want: "grpc"},
		{name: "case and padding are ignored",
			env: map[string]string{envProtocol: " GRPC "}, want: "grpc"},
		{name: "nothing set leaves the default", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{envMetricsProtocol, envProtocol} {
				t.Setenv(k, "")
				require.NoError(t, os.Unsetenv(k))
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			assert.Equal(t, tc.want, resolveProtocol(tc.configured))
		})
	}
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

// Staleness only exists once a repository has succeeded, so a newly added
// second destination that has never completed emits nothing at all. Alerting on
// the group's inventory does not catch it either: the sibling that is backing
// up fine satisfies any group-level join. Without a per-repository series there
// is nothing to alert on.
func TestRepositoryInfoCoversADestinationThatHasNeverSucceeded(t *testing.T) {
	now := time.Now()
	e, reader := newTestEmitter(t, fakeSource{snap: map[string]state.GroupRecord{
		"db": {Repositories: map[string]state.RepoRecord{
			"local":   {LastSuccess: now.Add(-time.Hour)},
			"offsite": {LastRun: &state.RepoOutcome{ID: "offsite", Result: state.ResultFailed}},
		}},
	}})
	e.ObserveInventory(models.NewBackupState(), nil)

	rm := collect(t, reader)
	info := findMetric(t, rm, "backup_repository_info")
	gauge, ok := info.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "backup_repository_info must be an int64 gauge")

	seen := map[string]int64{}
	for _, dp := range gauge.DataPoints {
		repo, present := dp.Attributes.Value(attribute.Key("repository"))
		require.True(t, present, "every sample carries a repository label to join on")
		group, present := dp.Attributes.Value(attribute.Key("group"))
		require.True(t, present, "every sample carries a group label to join on")
		seen[group.AsString()+"/"+repo.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"db/local": 1, "db/offsite": 1}, seen,
		"one sample per attempted repository, succeeded or not")

	// The point of the pair: offsite has an inventory sample and no staleness
	// sample, which is exactly what the alerting join keys on.
	stale := findMetric(t, rm, "backup_seconds_since_last_success")
	sg, ok := stale.Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.Len(t, sg.DataPoints, 1, "staleness is reported only for a repository that has succeeded")
	repo, _ := sg.DataPoints[0].Attributes.Value(attribute.Key("repository"))
	assert.Equal(t, "local", repo.AsString())
}

// A timeout or a signal leaves no per-repository breakdown to attribute the run
// to, but the configured destinations are still known. Counting those under an
// empty repository label drops precisely the unsuccessful attempts out of every
// repository-filtered dashboard.
func TestTerminatedRunsCountAgainstEachConfiguredRepository(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{})
	e.RecordRun("web", state.RunOutcome{
		Result:                 state.ResultTerminated,
		ConfiguredRepositories: []string{"local", "offsite"},
		// Repositories is empty: the run was killed before anything could be judged.
	})

	sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		got[attr(dp.Attributes, "repository")+"/"+attr(dp.Attributes, "result")] = dp.Value
	}
	assert.Equal(t, map[string]int64{
		"local/" + state.ResultTerminated:   1,
		"offsite/" + state.ResultTerminated: 1,
	}, got, "one sample per configured destination, none under an empty repository")
}

// The other half of the same branch: a config that never validated has no
// repository set at all, so there is nothing to attribute the run to and the
// group-level sample is the honest answer.
func TestARunWithNoKnownRepositoriesStillCountsOnce(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{})
	e.RecordRun("web", state.RunOutcome{Result: state.ResultFailed})

	sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, int64(1), sum.DataPoints[0].Value)
	assert.Empty(t, attr(sum.DataPoints[0].Attributes, "repository"))
}

// WithFromEnv advertises OTEL_SERVICE_NAME as the way to name a service, but a
// static detector listed after it silently overwrote the operator's value, so
// every manager instance reported under the same identity.
func TestServiceNameCanBeOverriddenFromTheEnvironment(t *testing.T) {
	serviceName := func(t *testing.T) string {
		t.Helper()
		reader := sdkmetric.NewManualReader()
		e, err := newEmitter(context.Background(), reader, "test",
			fakeSource{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		v, _ := rm.Resource.Set().Value("service.name")
		return v.AsString()
	}

	t.Run("the built-in name is the default", func(t *testing.T) {
		assert.Equal(t, "borgmatic-manager", serviceName(t))
	})

	t.Run("OTEL_SERVICE_NAME wins", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "backups-prod")
		assert.Equal(t, "backups-prod", serviceName(t))
	})

	t.Run("service.name in OTEL_RESOURCE_ATTRIBUTES wins", func(t *testing.T) {
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=backups-dr,deployment.environment=dr")
		assert.Equal(t, "backups-dr", serviceName(t))
	})

	t.Run("the version attribute survives an overridden name", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "backups-prod")
		reader := sdkmetric.NewManualReader()
		e, err := newEmitter(context.Background(), reader, "v9.9.9",
			fakeSource{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		v, _ := rm.Resource.Set().Value("service.version")
		assert.Equal(t, "v9.9.9", v.AsString())
	})
}

// A partly attributed failure: one destination is named in an error, another
// can be neither implicated nor confirmed. The run reached both. Counting only
// the judged one drops the ambiguous destination out of attempt-rate and
// failure dashboards, which is the opposite of what an unexplained repository
// deserves.
func TestUnattributedRepositoriesStillCountAsAnAttempt(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{})
	e.RecordRun("web", state.RunOutcome{
		Result:                 state.ResultFailed,
		ConfiguredRepositories: []string{"local", "offsite", "archive"},
		Repositories: []state.RepoOutcome{
			{ID: "local", Result: state.ResultOK},
			{ID: "offsite", Result: state.ResultFailed},
			// "archive" was neither implicated nor confirmed.
		},
	})

	sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		got[attr(dp.Attributes, "repository")+"/"+attr(dp.Attributes, "result")] = dp.Value
	}
	assert.Equal(t, map[string]int64{
		"local/" + state.ResultOK:        1,
		"offsite/" + state.ResultFailed:  1,
		"archive/" + state.ResultUnknown: 1,
	}, got, "every configured destination the run reached counts exactly once")
}

// The runner refuses to record a per-repository success for a maintenance-only
// cycle. The configured-repository fallback reached the same lie by another
// route: a manager configured with prune/compact/check and no create showed a
// steady stream of successful backups it never took.
func TestMaintenanceOnlyCyclesAreNotCountedAsBackups(t *testing.T) {
	t.Run("a successful maintenance cycle counts nothing", func(t *testing.T) {
		e, reader := newTestEmitter(t, fakeSource{})
		e.RecordRun("web", state.RunOutcome{
			Result:                 state.ResultOK,
			CreateAttempted:        false,
			ConfiguredRepositories: []string{"local", "offsite"},
		})
		rm := collect(t, reader)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				assert.NotEqual(t, "backup_runs_total", m.Name,
					"no archive was written, so no backup run happened")
			}
		}
	})

	// A check that keeps failing is exactly what wants alerting on, and counting
	// it is not a claim that anything was backed up.
	t.Run("a failed maintenance cycle still counts", func(t *testing.T) {
		e, reader := newTestEmitter(t, fakeSource{})
		e.RecordRun("web", state.RunOutcome{
			Result:                 state.ResultFailed,
			CreateAttempted:        false,
			ConfiguredRepositories: []string{"local"},
		})
		sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
		require.Len(t, sum.DataPoints, 1)
		assert.Equal(t, state.ResultFailed, attr(sum.DataPoints[0].Attributes, "result"))
		assert.Equal(t, "local", attr(sum.DataPoints[0].Attributes, "repository"))
	})

	t.Run("a real backup still counts", func(t *testing.T) {
		e, reader := newTestEmitter(t, fakeSource{})
		e.RecordRun("web", state.RunOutcome{
			Result:                 state.ResultOK,
			CreateAttempted:        true,
			ConfiguredRepositories: []string{"local"},
			Repositories:           []state.RepoOutcome{{ID: "local", Result: state.ResultOK}},
		})
		sum := findMetric(t, collect(t, reader), "backup_runs_total").Data.(metricdata.Sum[int64])
		require.Len(t, sum.DataPoints, 1)
		assert.Equal(t, int64(1), sum.DataPoints[0].Value)
	})
}

// The startup log exists to tell an operator where metrics are going. Deriving
// it separately from the exporter is how it comes to name a transport the
// exporter is not using.
func TestTheReportedTransportIsTheOneTheExporterUses(t *testing.T) {
	t.Run("config wins", func(t *testing.T) {
		assert.Equal(t, protocolGRPC, EffectiveProtocol(config.MetricsSettings{Protocol: "GRPC"}))
	})
	t.Run("the environment is reported when config is silent", func(t *testing.T) {
		t.Setenv(envProtocol, "grpc")
		assert.Equal(t, protocolGRPC, EffectiveProtocol(config.MetricsSettings{}))
	})
	t.Run("the default is named, not left blank", func(t *testing.T) {
		assert.Equal(t, protocolHTTP, EffectiveProtocol(config.MetricsSettings{}))
	})
	t.Run("endpoint falls back to the environment", func(t *testing.T) {
		t.Setenv(envEndpoint, "http://collector:4318")
		assert.Equal(t, "http://collector:4318", EffectiveEndpoint(config.MetricsSettings{}))
	})
	t.Run("the metrics-specific endpoint wins", func(t *testing.T) {
		t.Setenv(envEndpoint, "http://generic:4318")
		t.Setenv(envMetricsEndpoint, "http://specific:4318")
		assert.Equal(t, "http://specific:4318", EffectiveEndpoint(config.MetricsSettings{}))
	})
	t.Run("an unset endpoint says so rather than logging an empty string", func(t *testing.T) {
		assert.NotEmpty(t, EffectiveEndpoint(config.MetricsSettings{}))
	})
}

// The export decorator is the only thing that tells an operator whether metrics
// are actually reaching the collector: OTLP failures otherwise go to
// OpenTelemetry's global error handler as unstructured stderr, and successes are
// silent. It had no test at all.
func TestExportOutcomesReachTheLogs(t *testing.T) {
	newLogged := func(exportErr func() error) (*loggingExporter, *syncBuffer) {
		var buf syncBuffer
		return &loggingExporter{
			Exporter: &stubExporter{err: exportErr},
			logger:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}, &buf
	}
	ctx := context.Background()
	rm := &metricdata.ResourceMetrics{}

	t.Run("a failure warns every time", func(t *testing.T) {
		l, buf := newLogged(func() error { return errors.New("connection refused") })
		require.Error(t, l.Export(ctx, rm))
		require.Error(t, l.Export(ctx, rm))
		assert.Equal(t, 2, strings.Count(buf.String(), "level=WARN"),
			"a collector that stays down must keep saying so")
		assert.Contains(t, buf.String(), "connection refused", "including why")
	})

	t.Run("the first success is announced once, later ones are debug", func(t *testing.T) {
		l, buf := newLogged(func() error { return nil })
		require.NoError(t, l.Export(ctx, rm))
		require.NoError(t, l.Export(ctx, rm))
		require.NoError(t, l.Export(ctx, rm))
		assert.Equal(t, 1, strings.Count(buf.String(), "level=INFO"),
			"metrics are flowing is news once, not every interval")
		assert.Equal(t, 2, strings.Count(buf.String(), "level=DEBUG"))
	})

	t.Run("a recovery is announced, having been preceded by failures", func(t *testing.T) {
		fail := true
		l, buf := newLogged(func() error {
			if fail {
				return errors.New("down")
			}
			return nil
		})
		require.Error(t, l.Export(ctx, rm))
		fail = false
		require.NoError(t, l.Export(ctx, rm))
		assert.Contains(t, buf.String(), "level=WARN")
		assert.Contains(t, buf.String(), "level=INFO")
	})
}

// The count is logged as "series", and an operator reads it to sanity-check that
// what arrived is what they expect. Counting instruments instead reports 6 for a
// fan-out exporting dozens of series, which is not a small inaccuracy: nearly
// every metric here is per repository.
func TestExportedSeriesAreCountedNotInstruments(t *testing.T) {
	e, reader := newTestEmitter(t, fakeSource{snap: map[string]state.GroupRecord{
		"web": {Repositories: map[string]state.RepoRecord{
			"local":   {LastSuccess: time.Now(), LastStats: &state.RepoOutcome{Files: 1, OriginalBytes: 2, Measured: true}},
			"offsite": {LastSuccess: time.Now(), LastStats: &state.RepoOutcome{Files: 3, OriginalBytes: 4, Measured: true}},
		}},
	}})
	bs := models.NewBackupState()
	bs.AddVolume("web", models.VolumeInfo{Name: "v", HostPath: "/mnt/v"})
	e.ObserveInventory(bs, nil)

	rm := collect(t, reader)
	instruments := 0
	for _, sm := range rm.ScopeMetrics {
		instruments += len(sm.Metrics)
	}
	series := countDataPoints(&rm)

	assert.Greater(t, series, instruments,
		"two repositories and three size kinds make many more series than instruments")
	// backup_last_size_bytes alone is 2 repositories x 3 kinds.
	assert.GreaterOrEqual(t, series, 6)

	// An instrument whose aggregation the switch does not know counts as one
	// rather than zero, so adding an instrument type undercounts the log line
	// instead of making it disappear from it.
	unknown := metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{
		Metrics: []metricdata.Metrics{
			{Name: "histogram-ish", Data: metricdata.Histogram[int64]{
				DataPoints: []metricdata.HistogramDataPoint[int64]{{}, {}, {}},
			}},
		},
	}}}
	assert.Equal(t, 1, countDataPoints(&unknown))
}

// stubExporter is an sdkmetric.Exporter whose Export outcome the test controls.
type stubExporter struct {
	sdkmetric.Exporter
	err func() error
}

func (s *stubExporter) Export(context.Context, *metricdata.ResourceMetrics) error { return s.err() }

// syncBuffer is a bytes.Buffer safe for a logger writing from another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
