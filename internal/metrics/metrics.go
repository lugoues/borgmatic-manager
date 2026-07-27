// Package metrics exports native OpenTelemetry backup metrics over OTLP. It is
// daemon-only and best-effort: a broken exporter never fails a backup.
//
// One counter (backup_runs_total) is synchronous, incremented per repository as
// each run is recorded. Every other metric is an observable gauge read from the
// persisted schedule state at collection time, so a freshly restarted daemon
// re-reports last sizes and staleness immediately from disk instead of waiting
// for the next run.
package metrics

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"log/slog"

	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/state"
)

// meterName scopes the instruments to this program.
const meterName = "github.com/lugoues/borgmatic-manager"

// StateSource supplies persisted per-group records for the observable gauges.
// *state.ScheduleStore satisfies it.
type StateSource interface {
	Snapshot() map[string]state.GroupRecord
}

// Emitter owns the meter provider and instruments. Its RecordRun makes it a
// runner.Recorder (increments the run counter); ObserveInventory feeds the
// offline-volume gauge; the observable gauges pull from the StateSource.
type Emitter struct {
	provider *sdkmetric.MeterProvider
	logger   *slog.Logger
	source   StateSource
	now      func() time.Time

	runsTotal metric.Int64Counter

	// inventory is the latest cycle's per-group offline volume count and the
	// set of known groups (so an all-online group still reports 0). Guarded.
	mu        sync.Mutex
	offline   map[string]int
	allGroups map[string]struct{}
}

// New builds an Emitter and starts the periodic OTLP exporter. The caller must
// Shutdown it to flush. Returns an error only on unrecoverable setup failure
// (bad protocol, exporter construction); the daemon treats that as non-fatal.
func New(ctx context.Context, cfg config.MetricsSettings, version string, source StateSource, logger *slog.Logger) (*Emitter, error) {
	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Wrap the exporter so each push outcome surfaces in the daemon's structured
	// logs: without this, OTLP failures go only to OpenTelemetry's global error
	// handler (unstructured stderr) and successes are silent, leaving no way to
	// tell from the logs whether metrics are actually flowing.
	logged := &loggingExporter{Exporter: exp, logger: logger}
	return newEmitter(ctx, sdkmetric.NewPeriodicReader(logged), version, source, logger)
}

// loggingExporter decorates an OTLP exporter to log push outcomes: a one-time
// info on the first success (metrics are flowing), debug on later successes,
// and a warning on every failure (the collector is unreachable or rejecting).
type loggingExporter struct {
	sdkmetric.Exporter
	logger  *slog.Logger
	firstOK sync.Once
}

func (l *loggingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := l.Exporter.Export(ctx, rm)
	if err != nil {
		l.logger.Warn("metrics export failed; the OTLP collector is unreachable or rejecting", "error", err)
		return err
	}
	l.firstOK.Do(func() {
		l.logger.Info("metrics export succeeded; backup metrics are flowing to the collector", "series", countDataPoints(rm))
	})
	l.logger.Debug("metrics export succeeded", "series", countDataPoints(rm))
	return nil
}

// countDataPoints totals the series pushed, so the confirmation log shows the
// export was non-empty.
func countDataPoints(rm *metricdata.ResourceMetrics) int {
	n := 0
	for _, sm := range rm.ScopeMetrics {
		n += len(sm.Metrics)
	}
	return n
}

// newEmitter wires the meter provider around any reader (an OTLP periodic reader
// in production, a manual reader in tests) and registers the instruments.
func newEmitter(ctx context.Context, reader sdkmetric.Reader, version string, source StateSource, logger *slog.Logger) (*Emitter, error) {
	res, err := resource.New(ctx,
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName("borgmatic-manager"),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		// A resource error is not worth aborting metrics over: fall back to the
		// exporter default resource.
		logger.Warn("building metrics resource failed; using defaults", "error", err)
		res = resource.Default()
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	e := &Emitter{
		provider:  provider,
		logger:    logger,
		source:    source,
		now:       time.Now,
		offline:   map[string]int{},
		allGroups: map[string]struct{}{},
	}
	if err := e.register(provider.Meter(meterName)); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, err
	}
	return e, nil
}

// register creates the instruments and the single observable callback.
func (e *Emitter) register(m metric.Meter) error {
	var err error
	e.runsTotal, err = m.Int64Counter("backup_runs_total",
		metric.WithDescription("Count of borgmatic runs recorded, per repository and result."))
	if err != nil {
		return fmt.Errorf("creating backup_runs_total: %w", err)
	}

	lastSize, err := m.Int64ObservableGauge("backup_last_size_bytes",
		metric.WithDescription("Most recent archive size per repository, by kind (original, compressed, deduplicated)."),
		metric.WithUnit("By"))
	if err != nil {
		return fmt.Errorf("creating backup_last_size_bytes: %w", err)
	}
	lastDuration, err := m.Float64ObservableGauge("backup_last_duration_seconds",
		metric.WithDescription("Duration of the most recent successful backup per repository."),
		metric.WithUnit("s"))
	if err != nil {
		return fmt.Errorf("creating backup_last_duration_seconds: %w", err)
	}
	lastFiles, err := m.Int64ObservableGauge("backup_last_files",
		metric.WithDescription("File count of the most recent successful backup per repository."))
	if err != nil {
		return fmt.Errorf("creating backup_last_files: %w", err)
	}
	staleness, err := m.Float64ObservableGauge("backup_seconds_since_last_success",
		metric.WithDescription("Seconds since each repository last backed up successfully."),
		metric.WithUnit("s"))
	if err != nil {
		return fmt.Errorf("creating backup_seconds_since_last_success: %w", err)
	}
	groupInfo, err := m.Int64ObservableGauge("backup_group_info",
		metric.WithDescription("Always 1, once per known backup group. Without it a group that has never "+
			"reported is indistinguishable from one that no longer exists, because every other series here "+
			"only appears after a run."))
	if err != nil {
		return err
	}
	offlineVolumes, err := m.Int64ObservableGauge("backup_offline_volumes",
		metric.WithDescription("Number of a group's volumes whose container is currently offline."))
	if err != nil {
		return fmt.Errorf("creating backup_offline_volumes: %w", err)
	}

	_, err = m.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			e.observe(o, lastSize, lastFiles, offlineVolumes, groupInfo, lastDuration, staleness)
			return nil
		},
		lastSize, lastDuration, lastFiles, staleness, offlineVolumes, groupInfo,
	)
	if err != nil {
		return fmt.Errorf("registering metrics callback: %w", err)
	}
	return nil
}

// observe pulls current state and reports every gauge. Called by the SDK at
// each collection.
func (e *Emitter) observe(o metric.Observer,
	lastSize, lastFiles, offlineVolumes, groupInfo metric.Int64Observable,
	lastDuration, staleness metric.Float64Observable,
) {
	now := e.now()
	for group, rec := range e.source.Snapshot() {
		for id, rr := range rec.Repositories {
			repoAttrs := metric.WithAttributes(
				attribute.String("group", group),
				attribute.String("repository", id),
			)
			// last_* reflect the most recent successful backup: a failed run
			// carries no stats, so reporting its zeros would read as a shrink.
			if lr := rr.LastRun; lr != nil && lr.Result == state.ResultOK {
				o.ObserveInt64(lastSize, lr.OriginalBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "original")))
				o.ObserveInt64(lastSize, lr.CompressedBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "compressed")))
				o.ObserveInt64(lastSize, lr.DeduplicatedBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "deduplicated")))
				o.ObserveFloat64(lastDuration, float64(lr.DurationSeconds), repoAttrs)
				o.ObserveInt64(lastFiles, lr.Files, repoAttrs)
			}
			// A repository that has never succeeded has no meaningful staleness
			// value; its failing state is visible via backup_runs_total instead.
			if !rr.LastSuccess.IsZero() {
				o.ObserveFloat64(staleness, now.Sub(rr.LastSuccess).Seconds(), repoAttrs)
			}
		}
	}

	e.mu.Lock()
	for group := range e.allGroups {
		// One series per configured group, whether or not it has ever run. Every
		// other metric here springs into existence only after a backup, so
		// without this a group that has never succeeded looks exactly like one
		// that was deleted, and an alert on "no recent backup" cannot fire for
		// the group that most needs it.
		o.ObserveInt64(groupInfo, 1, metric.WithAttributes(attribute.String("group", group)))
		o.ObserveInt64(offlineVolumes, int64(e.offline[group]),
			metric.WithAttributes(attribute.String("group", group)))
	}
	e.mu.Unlock()
}

// RecordRun increments the run counter once per repository outcome, so a fan-out
// with one failed destination records both an ok and a failed sample. It does
// not persist anything: it composes with the schedule store as a Recorder.
func (e *Emitter) RecordRun(group string, o state.RunOutcome) {
	ctx := context.Background()
	if len(o.Repositories) == 0 {
		// A group with no per-repository breakdown (e.g. a config-invalid record)
		// still counts as one run at the group level.
		e.runsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("group", group), attribute.String("repository", ""), attribute.String("result", o.Result)))
		return
	}
	for _, ro := range o.Repositories {
		e.runsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("group", group), attribute.String("repository", ro.ID), attribute.String("result", ro.Result)))
	}
}

// ObserveInventory records this cycle's group inventory so the offline-volume
// gauge can report a count for every group (0 when all volumes are online).
func (e *Emitter) ObserveInventory(bs *models.BackupState, off *state.Offline) {
	if bs == nil {
		return
	}
	counts := make(map[string]int, len(bs.Groups))
	groups := make(map[string]struct{}, len(bs.Groups))
	for name, g := range bs.Groups {
		groups[name] = struct{}{}
		n := 0
		if off != nil {
			for _, v := range g.Volumes {
				if off.VolumeOffline(name, v.Name) {
					n++
				}
			}
		}
		counts[name] = n
	}
	e.mu.Lock()
	e.offline, e.allGroups = counts, groups
	e.mu.Unlock()
}

// Shutdown flushes buffered metrics and stops the exporter.
func (e *Emitter) Shutdown(ctx context.Context) error {
	return e.provider.Shutdown(ctx)
}

// newExporter builds an OTLP metric exporter for the configured protocol. An
// empty endpoint lets the exporter fall back to OTEL_EXPORTER_OTLP_ENDPOINT and
// then the OTLP default host and port.
func newExporter(ctx context.Context, cfg config.MetricsSettings) (sdkmetric.Exporter, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case "grpc":
		var opts []otlpmetricgrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case "", "http", "http/protobuf":
		var opts []otlpmetrichttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown metrics protocol %q (want \"http\" or \"grpc\")", cfg.Protocol)
	}
}
