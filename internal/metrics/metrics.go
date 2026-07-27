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
	"errors"
	"fmt"
	"os"
	"regexp"
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

	runsTotal      metric.Int64Counter
	groupRunsTotal metric.Int64Counter

	// inventory is the latest cycle's per-group offline volume count and the
	// set of known groups (so an all-online group still reports 0). Guarded.
	mu        sync.Mutex
	offline   map[string]int
	allGroups map[string]struct{}
	// repos is each group's currently configured repository ids, reported after
	// generation. Nil until a cycle reports one.
	repos map[string]map[string]string
	// observed records that a cycle has reported an inventory, which an empty
	// allGroups cannot: the last group being removed and no cycle having run yet
	// are the same empty map and opposite situations.
	observed bool
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
		l.logger.Warn("metrics export failed; the OTLP collector is unreachable or rejecting",
			"error", redactURLsIn(err.Error()))
		return err
	}
	// The first success is news and the rest are not, so exactly one of the two
	// lines is emitted: logging both for the same export reads as two events.
	// The count is computed once either way; it walks every data point, and this
	// runs on every export interval for the life of the daemon.
	series := countDataPoints(rm)
	announced := false
	l.firstOK.Do(func() {
		announced = true
		l.logger.Info("metrics export succeeded; backup metrics are flowing to the collector", "series", series)
	})
	if !announced {
		l.logger.Debug("metrics export succeeded", "series", series)
	}
	return nil
}

// countDataPoints totals the series pushed, so the confirmation log shows the
// export was non-empty.
// urlInText matches a URL in a message, quoted or bare, in one alternation.
//
// Matched on ":/" rather than on "://", because an endpoint can be malformed in
// either direction: "https:/collector?token=..." has one slash and reaches the
// exporter unvalidated just as readily. RedactEndpoint treats a value it cannot
// parse as unprintable, so a broader match lands on the safer branch, and a value
// with nothing to hide passes through unchanged.
//
// Matched on the separator rather than on a spelled-out scheme, because an endpoint
// whose scheme is malformed still carries its query: an exporter rejecting
// "ht!tp://collector/path?token=..." returns the whole value, and a pattern
// anchored on "https?://" walked straight past it. RedactEndpoint handles a
// value it cannot parse by refusing to print it, so widening what reaches it is
// safe in the direction that matters.
//
// The quoted form comes first so a URL the transport wrapped in quotes is
// bounded by its closing quote, which is the shape Go's net/http errors use:
// Post "https://host/path?query": ... The bare form ends only at whitespace,
// deliberately: a query string may legally contain "," ";" "'" and ")", and
// stopping at those left the tail of a token in the message, so the log read as
// redacted while still carrying the secret. Swallowing a trailing full stop from
// prose is the cheaper mistake.
//
// One alternation rather than two passes, because a second pass re-matches what
// the first already redacted and eats the closing quote with it, taking the rest
// of the message's punctuation along.
var urlInText = regexp.MustCompile(`"[^"\s]*:/[^"]*"|\S*:/+\S+`)

// RedactErrorText strips credentials from any URL inside an error message, for
// callers that log an error this package returned before it reached a decorator.
func RedactErrorText(err error) string {
	if err == nil {
		return ""
	}
	return redactURLsIn(err.Error())
}

// redactURLsIn strips credentials from any URL inside a message.
//
// The transport builds its own error text, and for an HTTP exporter that text
// can contain the full request URL: with a collector authenticated by a query
// token, an unreachable collector writes that token to the journal on every
// export interval, which is the loudest possible way to leak it. Redacting the
// startup log alone covered the one line nobody was worried about.
func redactURLsIn(msg string) string {
	return urlInText.ReplaceAllStringFunc(msg, func(match string) string {
		if strings.HasPrefix(match, `"`) {
			return `"` + config.RedactEndpoint(strings.Trim(match, `"`)) + `"`
		}
		return config.RedactEndpoint(match)
	})
}

// countDataPoints counts the exported time series, not the instruments. The two
// differ by an order of magnitude here: nearly every metric is per repository,
// and backup_last_size_bytes is per repository per size kind, so a handful of
// instruments is dozens of series. The number exists to tell an operator that
// metrics are flowing and roughly how much, which counting instruments does not.
//
// The type switch covers the aggregations this program emits; anything else
// counts as one rather than zero, so an added instrument is undercounted rather
// than invisible.
func countDataPoints(rm *metricdata.ResourceMetrics) int {
	n := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				n += len(d.DataPoints)
			case metricdata.Gauge[float64]:
				n += len(d.DataPoints)
			case metricdata.Sum[int64]:
				n += len(d.DataPoints)
			case metricdata.Sum[float64]:
				n += len(d.DataPoints)
			default:
				n++
			}
		}
	}
	return n
}

// instanceID identifies this process among the ones exporting these metrics.
//
// backup_runs_total is cumulative, and a one-shot "run" is supported while the
// daemon is up, so two processes can export the same metric with the same
// attributes at once. Each SDK owns its own counter with its own start time, so
// without a distinguishing resource attribute a collector sees one stream from
// two producers: the manual run reads as a counter reset, or is overwritten by
// the daemon's next push. Both lose the manual backup, which is the run an
// operator is most likely to be watching.
//
// The pid alone would be reused across reboots, so it is paired with the process
// start time. Deriving it rather than minting a random id keeps a restarted
// daemon distinguishable from the one it replaced while staying reproducible
// within a process.
func instanceID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), processStart.UnixNano())
}

// processStart is stamped once so every call in this process agrees.
var processStart = time.Now()

// newEmitter wires the meter provider around any reader (an OTLP periodic reader
// in production, a manual reader in tests) and registers the instruments.
func newEmitter(ctx context.Context, reader sdkmetric.Reader, version string, source StateSource, logger *slog.Logger) (*Emitter, error) {
	// Detector order is significant: later detectors override earlier ones. The
	// built-in identity goes first so it acts as a default, and WithFromEnv last
	// so OTEL_SERVICE_NAME and service.name in OTEL_RESOURCE_ATTRIBUTES actually
	// take effect. Reversed, the static name silently overwrote the configured
	// one and every manager instance reported as the same service. The version
	// attribute survives unless the environment deliberately replaces it.
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName("borgmatic-manager"),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(instanceID()),
		),
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
	)
	if err != nil {
		// A resource error is not worth aborting metrics over, but the fallback
		// has to keep the identity attributes. service.instance.id is what stops
		// a one-shot run and the daemon being read as one cumulative stream, and
		// dropping it because OTEL_RESOURCE_ATTRIBUTES had a malformed entry
		// would reintroduce that silently, at the moment the operator is least
		// likely to be looking.
		logger.Warn("building metrics resource failed; keeping the service identity and using defaults for the rest",
			"error", err)
		identity := resource.NewSchemaless(
			semconv.ServiceName("borgmatic-manager"),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(instanceID()),
		)
		merged, mergeErr := resource.Merge(resource.Default(), identity)
		if mergeErr != nil {
			logger.Warn("merging the fallback metrics resource failed; exporting without a service identity",
				"error", mergeErr)
			merged = resource.Default()
		}
		res = merged
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

	e.groupRunsTotal, err = m.Int64Counter("backup_group_runs_total",
		metric.WithDescription("Count of borgmatic runs recorded, per group and result. The group's own verdict, "+
			"which is not always the per-repository one: a run whose create reached every destination still fails "+
			"when a later prune, compact or check does."))
	if err != nil {
		return fmt.Errorf("creating backup_group_runs_total: %w", err)
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
	repoInfo, err := m.Int64ObservableGauge("backup_repository_info",
		metric.WithDescription("Always 1, once per repository the group has attempted. Staleness is only "+
			"reported for a repository that has succeeded at least once, so without this a destination that "+
			"has never produced an archive has no series to alert on."))
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
			e.observe(o, lastSize, lastFiles, offlineVolumes, groupInfo, repoInfo, lastDuration, staleness)
			return nil
		},
		lastSize, lastDuration, lastFiles, staleness, offlineVolumes, groupInfo, repoInfo,
	)
	if err != nil {
		return fmt.Errorf("registering metrics callback: %w", err)
	}
	return nil
}

// observe pulls current state and reports every gauge. Called by the SDK at
// each collection.
func (e *Emitter) observe(o metric.Observer,
	lastSize, lastFiles, offlineVolumes, groupInfo, repoInfo metric.Int64Observable,
	lastDuration, staleness metric.Float64Observable,
) {
	now := e.now()

	e.mu.Lock()
	observed := e.observed
	configuredRepos := make(map[string]map[string]string, len(e.repos))
	for group, byID := range e.repos {
		inner := make(map[string]string, len(byID))
		for id, path := range byID {
			inner[id] = path
		}
		configuredRepos[group] = inner
	}
	known := make(map[string]struct{}, len(e.allGroups))
	for g := range e.allGroups {
		known[g] = struct{}{}
	}
	offline := make(map[string]int, len(e.offline))
	for g, n := range e.offline {
		offline[g] = n
	}
	e.mu.Unlock()

	for group, rec := range e.source.Snapshot() {
		// A group removed from the inventory keeps its schedule record for two
		// absent cycles, so a redeploy blip does not wipe its history. Exporting
		// gauges from that record while backup_group_info has already dropped the
		// group puts the two inventory levels in contradiction: the documented
		// join keeps firing for a destination that no longer exists, and does so
		// during every transient disappearance. The record is the right thing to
		// keep and the wrong thing to export.
		//
		// Filtered once an inventory has actually been observed, so a collection
		// landing before the first cycle still reports from disk, which is the
		// point of reading these gauges from persisted state. The flag is what
		// carries that distinction: removing the last group leaves an inventory
		// that is empty and observed, which an empty map alone cannot tell from
		// one that was never filled.
		if observed {
			if _, ok := known[group]; !ok {
				continue
			}
		}
		// A schedule file written before per-repository records existed has a
		// group last-success and no repositories at all. The scheduler honours
		// that success and may skip the group for a whole period, so without
		// standing in for it the documented join reports every upgraded group as
		// never attempted until its next backup happens to run.
		//
		// The empty repository label is this codebase's existing way of saying
		// "the group, with no destination attributable", which is exactly what a
		// legacy record knows. It disappears the first time the group runs.
		//
		// Only while the inventory is unknown. A current group changed to
		// "repositories: []" also has no repository records and keeps its older
		// group success, and standing in for that one would contradict an
		// inventory that has already been reported as empty, suppressing the
		// never-attempted alert for a group that now backs up nowhere.
		_, inventoryKnown := configuredRepos[group]
		if !inventoryKnown && len(rec.Repositories) == 0 && !rec.LastSuccess.IsZero() {
			legacyAttrs := metric.WithAttributes(
				attribute.String("group", group), attribute.String("repository", ""))
			o.ObserveInt64(repoInfo, 1, legacyAttrs)
			o.ObserveFloat64(staleness, now.Sub(rec.LastSuccess).Seconds(), legacyAttrs)
		}
		for id, rr := range rec.Repositories {
			// A repository removed from a group the group still has. The record
			// survives until the next run reconciles it, and that run may be a
			// whole period away, so the reconciliation has to happen here too.
			repoAttrs := metric.WithAttributes(
				attribute.String("group", group),
				attribute.String("repository", id),
			)
			repointed := false
			if configured, ok := configuredRepos[group]; ok {
				path, stillConfigured := configured[id]
				if !stillConfigured {
					continue
				}
				// The id survives a repoint but the history does not belong to
				// the new destination. Until a run reconciles the record, its
				// stored path is what says which one these numbers describe.
				repointed = path != "" && rr.Path != "" && !state.SameDestination(rr.Path, path)
			}
			if repointed {
				// The numbers are the old destination's, but the repository is
				// configured and has never completed, which is precisely what the
				// documented join exists to report. Suppressing the whole record
				// hid it behind a sibling's info series until the next run.
				o.ObserveInt64(repoInfo, 1, repoAttrs)
				continue
			}
			// last_* reflect the most recent successful backup: a failed run
			// carries no stats, so reporting its zeros would read as a shrink.
			// Read from the last outcome that measured something, not the last
			// run. A later failure replaces LastRun outright, and a
			// probe-confirmed success carries no stats at all, so reading LastRun
			// either stopped reporting sizes or reported zeros — and a dataset
			// that appears to have shrunk to nothing is a worse lie than one that
			// stops updating.
			if lr := rr.LastStats; lr != nil {
				o.ObserveInt64(lastSize, lr.OriginalBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "original")))
				o.ObserveInt64(lastSize, lr.CompressedBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "compressed")))
				o.ObserveInt64(lastSize, lr.DeduplicatedBytes, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("kind", "deduplicated")))
				o.ObserveFloat64(lastDuration, lr.DurationSeconds, repoAttrs)
				o.ObserveInt64(lastFiles, lr.Files, repoAttrs)
			}
			// One series per repository the group has attempted, so an alert can
			// join against it. A repository that has never succeeded has no
			// meaningful staleness value, and a fan-out group's info series says
			// nothing about which destination is behind: without a per-repository
			// sample, a newly added second repository that has never once
			// completed is invisible to any staleness rule.
			o.ObserveInt64(repoInfo, 1, repoAttrs)
			if !rr.LastSuccess.IsZero() {
				o.ObserveFloat64(staleness, now.Sub(rr.LastSuccess).Seconds(), repoAttrs)
			}
		}

		// A repository added to an already-successful group has no record yet,
		// and repository settings do not make the group due, so none may appear
		// for a whole period. Iterating records alone left the new destination
		// with no series at all, and the group-level fallback cannot reveal it
		// because a healthy sibling already supplies the group's info series.
		for id := range configuredRepos[group] {
			if _, known := rec.Repositories[id]; known {
				continue
			}
			o.ObserveInt64(repoInfo, 1, metric.WithAttributes(
				attribute.String("group", group), attribute.String("repository", id)))
		}
	}

	for group := range known {
		// One series per configured group, whether or not it has ever run. Every
		// other metric here springs into existence only after a backup, so
		// without this a group that has never succeeded looks exactly like one
		// that was deleted, and an alert on "no recent backup" cannot fire for
		// the group that most needs it.
		o.ObserveInt64(groupInfo, 1, metric.WithAttributes(attribute.String("group", group)))
		o.ObserveInt64(offlineVolumes, int64(offline[group]),
			metric.WithAttributes(attribute.String("group", group)))
	}
}

// counterResult maps a stored outcome onto the counter's documented label set.
//
// State keeps the specific status because status and inspect want the detail;
// the counter has a fixed vocabulary that dashboards and alert rules select on.
// Exporting a status like "config-invalid" raw means a rule matching
// result="failed" silently misses exactly the runs that never got off the
// ground, so anything outside the vocabulary lands on "failed": it did not
// succeed, and being wrong about which kind of failure is far better than being
// invisible.
func counterResult(result string) string {
	switch result {
	case state.ResultOK, state.ResultFailed, state.ResultTerminated, state.ResultUnknown:
		return result
	default:
		return state.ResultFailed
	}
}

// RecordRun increments the run counter once per repository outcome, so a fan-out
// with one failed destination records both an ok and a failed sample. It does
// not persist anything: it composes with the schedule store as a Recorder.
func (e *Emitter) RecordRun(group string, o state.RunOutcome) {
	ctx := context.Background()

	// The group's own verdict, before the backup-specific guard below, because
	// this counter is about runs and that guard is about backups.
	//
	// It is a different question from any destination's: when create reaches
	// every repository and a later prune, compact or check fails, every
	// per-repository sample is correctly ok and the run is still a failure.
	// Derived from the repository samples alone, a recurring maintenance failure
	// is invisible to anything selecting failed runs.
	//
	// A maintenance-only cycle counts here and not in backup_runs_total. Counting
	// its failures and dropping its successes would have been the worst of both:
	// any rate over this counter would read as a permanent 100% failure for a
	// manager configured that way. "Did this run succeed" and "was anything
	// backed up" are separate questions, and each counter answers one of them.
	e.groupRunsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("group", group), attribute.String("result", counterResult(o.Result))))

	// A maintenance-only cycle (prune, compact, check, with no create) exits zero
	// having written no archive anywhere. The runner already refuses to record a
	// per-repository success for one; counting it here would put the same lie in
	// the counter by another route, and a manager configured that way would show
	// a steady stream of successful backups it never took.
	//
	// A maintenance cycle backs nothing up whether it succeeds or fails, so it
	// belongs in neither direction of the per-repository backup counter: its
	// successes would claim backups that never happened, and its failures would
	// weigh against a backup success rate computed from this counter while
	// duplicating what the group counter already records. A failing check is
	// still alertable, on backup_group_runs_total, which is where the README
	// points maintenance alerts.
	if !o.CreateAttempted {
		return
	}

	if len(o.Repositories) == 0 {
		// A timeout, a signal, or a failure that cannot be attributed leaves
		// Repositories empty while ConfiguredRepositories still names every
		// destination. Counting those under repository="" would drop exactly the
		// unsuccessful attempts out of any repository-filtered dashboard, which
		// is the opposite of what the counter is for.
		if len(o.ConfiguredRepositories) > 0 {
			for _, id := range o.ConfiguredRepositories {
				e.runsTotal.Add(ctx, 1, metric.WithAttributes(
					attribute.String("group", group), attribute.String("repository", id), attribute.String("result", counterResult(o.Result))))
			}
			return
		}
		// Nothing to attribute it to. A config-invalid run does name its
		// destinations (the manager wrote the config that failed validation) and
		// the runner passes them, so this is now only reached when the group has
		// no usable repository set at all: generation itself failed, or the
		// config named none.
		e.runsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("group", group), attribute.String("repository", ""), attribute.String("result", counterResult(o.Result))))
		return
	}
	judged := make(map[string]bool, len(o.Repositories))
	for _, ro := range o.Repositories {
		judged[ro.ID] = true
		e.runsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("group", group), attribute.String("repository", ro.ID), attribute.String("result", counterResult(ro.Result))))
	}
	// A partly attributed failure: one destination is named in an error while
	// another can be neither implicated nor confirmed. The run reached both, so
	// counting only the judged ones drops the ambiguous destination out of
	// attempt-rate and failure dashboards entirely, which is the opposite of what
	// an unexplained repository deserves. State deliberately leaves such a
	// repository untouched; the counter still records that an attempt happened.
	for _, id := range o.ConfiguredRepositories {
		if !judged[id] {
			e.runsTotal.Add(ctx, 1, metric.WithAttributes(
				attribute.String("group", group), attribute.String("repository", id),
				attribute.String("result", state.ResultUnknown)))
		}
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
	e.offline, e.allGroups, e.observed = counts, groups, true
	e.mu.Unlock()
}

// ObserveRepositories records which repositories each group currently
// configures, so the gauges can drop one that has been removed.
//
// A run is too late to learn this. Scheduler dueness is computed from the volume
// and database fingerprint, not from repository settings, so a group whose
// repository list changed is not due any sooner: without this it keeps exporting
// the removed destination's inventory, staleness and last-value series for a
// whole backup period, and the documented alert keeps firing for something that
// is no longer configured.
func (e *Emitter) ObserveRepositories(repos map[string]map[string]string) {
	copied := make(map[string]map[string]string, len(repos))
	for group, byID := range repos {
		inner := make(map[string]string, len(byID))
		for id, path := range byID {
			inner[id] = path
		}
		copied[group] = inner
	}
	e.mu.Lock()
	e.repos = copied
	e.mu.Unlock()
}

// Shutdown flushes buffered metrics and stops the exporter.
//
// The error is redacted here rather than at each caller. The exporter decorator
// sanitizes its own warning and then returns the original error, which reaches
// whatever logs a failed shutdown: on a one-shot run that is the only export
// there is, so the collector URL and any token in it would go to the journal by
// the shortest path available.
func (e *Emitter) Shutdown(ctx context.Context) error {
	if err := e.provider.Shutdown(ctx); err != nil {
		return errors.New(redactURLsIn(err.Error()))
	}
	return nil
}

// OTLP transport names and the standard environment variables that select one.
const (
	protocolGRPC       = "grpc"
	protocolHTTP       = "http"
	protocolHTTPProto  = "http/protobuf"
	envMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

// newExporter builds an OTLP metric exporter for the configured protocol. An
// empty endpoint lets the exporter fall back to OTEL_EXPORTER_OTLP_ENDPOINT and
// then the OTLP default host and port.
func newExporter(ctx context.Context, cfg config.MetricsSettings) (sdkmetric.Exporter, error) {
	// The resolved value, not the configured one: when the protocol came from
	// the environment, cfg.Protocol is empty and reporting it sends an operator
	// to a blank config field instead of the variable that caused the failure.
	protocol := resolveProtocol(cfg.Protocol)
	switch protocol {
	case protocolGRPC:
		var opts []otlpmetricgrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case "", protocolHTTP, protocolHTTPProto:
		var opts []otlpmetrichttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown metrics protocol %q (want \"http\" or \"grpc\"); "+
			"set it in metrics.protocol or in OTEL_EXPORTER_OTLP_METRICS_PROTOCOL / OTEL_EXPORTER_OTLP_PROTOCOL", protocol)
	}
}

// EffectiveProtocol reports the OTLP transport that will actually be used, after
// the config and the standard environment variables are taken into account. It
// exists so a startup log cannot claim a different transport from the one the
// exporter was built with: re-deriving it at the call site is how those drift.
func EffectiveProtocol(cfg config.MetricsSettings) string {
	if p := resolveProtocol(cfg.Protocol); p != "" {
		return p
	}
	return protocolHTTP
}

// EffectiveEndpoint reports the endpoint that will actually be used, redacted
// for logging. An empty config endpoint lets the exporter fall back to the
// standard environment variables, so report those rather than logging a blank.
//
// Redacted because this value goes to the journal, which is readable by more
// people than the config is. An authenticated collector is configured exactly as
// the OTLP spec allows, with userinfo in the URL or a token in the query string,
// and printing it whole hands that credential to every journal reader. The
// scheme, host and path are what an operator needs to see; the secret is not.
func EffectiveEndpoint(cfg config.MetricsSettings) string {
	return config.RedactEndpoint(rawEndpoint(cfg))
}

func rawEndpoint(cfg config.MetricsSettings) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	for _, key := range []string{envMetricsEndpoint, envEndpoint} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "(OTLP default)"
}

// resolveProtocol picks the OTLP transport, falling back to the standard
// environment variables when the config does not name one.
//
// Each transport exporter reads the OTEL_* endpoint and TLS settings for
// itself, but neither can switch into the other implementation, so choosing
// which one to construct has to happen here. Without this a deployment relying
// on the documented OTEL_EXPORTER_OTLP_PROTOCOL=grpc sent HTTP to a gRPC
// collector and exported nothing at all.
func resolveProtocol(configured string) string {
	if p := strings.ToLower(strings.TrimSpace(configured)); p != "" {
		return p
	}
	// Metrics-specific wins over the generic one, as the OTLP spec requires.
	for _, key := range []string{envMetricsProtocol, envProtocol} {
		if p := strings.ToLower(strings.TrimSpace(os.Getenv(key))); p != "" {
			return p
		}
	}
	return ""
}
