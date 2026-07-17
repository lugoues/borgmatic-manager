package events

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// defaultDebounce coalesces the burst of events a single `docker compose up`
// produces into one re-discovery.
const defaultDebounce = 5 * time.Second

// maxDebounceRatio caps the coalescing window from the burst's first event:
// sustained churn would otherwise reset the timer forever and strand new containers.
const maxDebounceRatio = 6

// Listener watches a ContainerRuntime event stream and coalesces rapid events
// into debounced re-discovery triggers.
type Listener struct {
	rt               runtime.ContainerRuntime
	logger           *slog.Logger
	debounceDuration time.Duration
	maxDebounceWait  time.Duration
	backoffDuration  time.Duration
}

// NewListener creates a Listener with the default debounce interval.
func NewListener(rt runtime.ContainerRuntime, logger *slog.Logger) *Listener {
	return NewListenerWithDebounce(rt, logger, defaultDebounce)
}

// NewListenerWithDebounce creates a Listener with a custom debounce, for tests.
func NewListenerWithDebounce(rt runtime.ContainerRuntime, logger *slog.Logger, debounce time.Duration) *Listener {
	return &Listener{
		rt:               rt,
		logger:           logger,
		debounceDuration: debounce,
		maxDebounceWait:  debounce * maxDebounceRatio,
		backoffDuration:  5 * time.Second,
	}
}

// Listen returns a buffered(1) trigger channel (sends never block, extras
// drop) that closes when the context is cancelled.
func (l *Listener) Listen(ctx context.Context) <-chan struct{} {
	sender := &triggerSender{ch: make(chan struct{}, 1)}

	go func() {
		defer sender.close()
		l.reconnectLoop(ctx, sender)
	}()

	return sender.ch
}

// triggerSender serializes sends against close: a debounce timer firing at
// shutdown must not send on the closed channel.
type triggerSender struct {
	mu     sync.Mutex
	ch     chan struct{}
	closed bool
}

// send delivers a trigger without blocking and without racing close.
func (t *triggerSender) send() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	select {
	case t.ch <- struct{}{}:
	default:
	}
}

func (t *triggerSender) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	close(t.ch)
}

// reconnectLoop repeatedly connects to the event stream and processes events.
// On disconnect or error it waits backoffDuration before reconnecting.
func (l *Listener) reconnectLoop(ctx context.Context, sender *triggerSender) {
	reconnected := false
	for {
		if ctx.Err() != nil {
			return
		}

		eventCh, errCh := l.rt.EventStream(ctx)
		if reconnected {
			// Events during the disconnect were unobservable; one unconditional
			// trigger closes the gap.
			l.logger.Info("event stream reconnected; triggering re-discovery to cover the gap")
			sender.send()
		}
		reconnected = true
		l.processStream(ctx, eventCh, errCh, sender)

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
func (l *Listener) processStream(ctx context.Context, eventCh <-chan runtime.Event, errCh <-chan error, sender *triggerSender) {
	var (
		timer *time.Timer
		// fire is non-nil exactly while a debounce is pending, so the loop
		// knows when a burst ends and the next event starts a fresh one.
		fire       <-chan time.Time
		burstStart time.Time
	)

	// A non-nil timer means a trigger is owed: on disconnect it must fire (an
	// observed event must not be lost); on shutdown it is dropped.
	defer func() {
		if timer == nil {
			return
		}
		timer.Stop()
		if ctx.Err() == nil {
			sender.send()
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

			now := time.Now()
			if fire == nil {
				burstStart = now // first event of a new burst
			} else {
				timer.Stop() // extend the current window
			}

			// Coalesce, but never past maxDebounceWait from the burst's first event,
			// or sustained churn pushes the window out forever.
			wait := l.debounceDuration
			if remaining := l.maxDebounceWait - now.Sub(burstStart); remaining < wait {
				wait = max(remaining, 0)
			}
			timer = time.NewTimer(wait)
			fire = timer.C

		case <-fire:
			timer, fire = nil, nil
			sender.send()

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
