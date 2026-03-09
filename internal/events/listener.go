package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Listener watches a ContainerRuntime event stream and coalesces rapid events
// into debounced re-discovery triggers.
type Listener struct {
	rt               runtime.ContainerRuntime
	logger           *slog.Logger
	debounceDuration time.Duration
	backoffDuration  time.Duration
}

// NewListener creates a Listener with the default 5-second debounce interval.
func NewListener(rt runtime.ContainerRuntime, logger *slog.Logger) *Listener {
	return &Listener{
		rt:               rt,
		logger:           logger,
		debounceDuration: 5 * time.Second,
		backoffDuration:  5 * time.Second,
	}
}

// NewListenerWithDebounce creates a Listener with a custom debounce, for tests.
func NewListenerWithDebounce(rt runtime.ContainerRuntime, logger *slog.Logger, debounce time.Duration) *Listener {
	return &Listener{
		rt:               rt,
		logger:           logger,
		debounceDuration: debounce,
		backoffDuration:  5 * time.Second,
	}
}

// Listen returns a buffered(1) trigger channel (sends never block, extras
// drop) that closes when the context is cancelled.
func (l *Listener) Listen(ctx context.Context) <-chan struct{} {
	triggerCh := make(chan struct{}, 1)

	go func() {
		defer close(triggerCh)
		l.reconnectLoop(ctx, triggerCh)
	}()

	return triggerCh
}

// reconnectLoop repeatedly connects to the event stream and processes events.
// On disconnect or error it waits backoffDuration before reconnecting.
func (l *Listener) reconnectLoop(ctx context.Context, triggerCh chan struct{}) {
	for {
		if ctx.Err() != nil {
			return
		}

		eventCh, errCh := l.rt.EventStream(ctx)
		l.processStream(ctx, eventCh, errCh, triggerCh)

		// If context is done, exit without backoff.
		if ctx.Err() != nil {
			return
		}

		l.logger.Warn("event stream disconnected, reconnecting", "backoff", l.backoffDuration)
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.backoffDuration):
		}
	}
}

// processStream reads from the event and error channels until one of them
// indicates a disconnect or the context is cancelled.
func (l *Listener) processStream(ctx context.Context, eventCh <-chan runtime.Event, errCh <-chan error, triggerCh chan struct{}) {
	var debounceTimer *time.Timer

	defer func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			l.logger.Debug("received event", "type", evt.Type, "action", evt.Action, "actor", evt.Actor)

			// Reset or start debounce timer.
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(l.debounceDuration, func() {
				// Non-blocking send: drop if trigger already pending.
				select {
				case triggerCh <- struct{}{}:
				default:
				}
			})

		case err, ok := <-errCh:
			if !ok {
				// Error channel closed: treat as disconnect.
				return
			}
			l.logger.Warn("event stream error", "error", err)
			return
		}
	}
}
