package events

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

const testDebounce = 100 * time.Millisecond

// setupMockStream creates a mock that returns controlled channels on EventStream.
func setupMockStream(m *runtime.MockRuntime) (chan runtime.Event, chan error) {
	eventCh := make(chan runtime.Event)
	errCh := make(chan error)
	readEventCh := (<-chan runtime.Event)(eventCh)
	readErrCh := (<-chan error)(errCh)
	m.On("EventStream", mock.Anything).Return(readEventCh, readErrCh).Once()
	return eventCh, errCh
}

func TestListener_SingleEvent(t *testing.T) {
	m := &runtime.MockRuntime{}
	eventCh, _ := setupMockStream(m)

	logger := slog.Default()
	l := NewListenerWithDebounce(m, logger, testDebounce)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	triggerCh := l.Listen(ctx)

	// Send one event.
	eventCh <- runtime.Event{Type: "container", Action: "create", Actor: "abc123"}

	// Should receive a trigger after debounce.
	select {
	case _, ok := <-triggerCh:
		assert.True(t, ok, "trigger channel should deliver a value, not be closed")
	case <-time.After(2 * time.Second):
		t.Fatal("expected trigger after debounce, got timeout")
	}

	m.AssertExpectations(t)
}

func TestListener_DebounceCoalesces(t *testing.T) {
	m := &runtime.MockRuntime{}
	eventCh, _ := setupMockStream(m)

	logger := slog.Default()
	l := NewListenerWithDebounce(m, logger, testDebounce)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	triggerCh := l.Listen(ctx)

	// Send 3 rapid events.
	eventCh <- runtime.Event{Type: "container", Action: "create", Actor: "a"}
	eventCh <- runtime.Event{Type: "volume", Action: "remove", Actor: "b"}
	eventCh <- runtime.Event{Type: "container", Action: "remove", Actor: "c"}

	// Should receive exactly 1 trigger.
	select {
	case <-triggerCh:
		// Good, got the coalesced trigger.
	case <-time.After(2 * time.Second):
		t.Fatal("expected trigger after debounce, got timeout")
	}

	// Verify no second trigger arrives.
	select {
	case <-triggerCh:
		t.Fatal("expected no second trigger, debounce should coalesce")
	case <-time.After(testDebounce * 2):
		// Good, no extra trigger.
	}

	m.AssertExpectations(t)
}

func TestListener_Reconnect(t *testing.T) {
	m := &runtime.MockRuntime{}

	// First stream: will be closed to simulate disconnect.
	eventCh1 := make(chan runtime.Event)
	errCh1 := make(chan error)
	m.On("EventStream", mock.Anything).Return((<-chan runtime.Event)(eventCh1), (<-chan error)(errCh1)).Once()

	// Second stream: reconnected.
	eventCh2 := make(chan runtime.Event)
	errCh2 := make(chan error)
	m.On("EventStream", mock.Anything).Return((<-chan runtime.Event)(eventCh2), (<-chan error)(errCh2)).Once()

	logger := slog.Default()
	// Use a very short debounce and we'll need backoff to also be short for testing.
	l := NewListenerWithDebounce(m, logger, testDebounce)
	// Override backoff for test speed (we'll use a small backoff internally).
	l.backoffDuration = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	triggerCh := l.Listen(ctx)

	// Close first stream to simulate disconnect.
	close(eventCh1)
	close(errCh1)

	// Give time for reconnect.
	time.Sleep(200 * time.Millisecond)

	// Send event on second stream.
	eventCh2 <- runtime.Event{Type: "container", Action: "create", Actor: "reconnected"}

	// Should receive trigger from second stream.
	select {
	case <-triggerCh:
		// Good, reconnect worked.
	case <-time.After(2 * time.Second):
		t.Fatal("expected trigger after reconnect, got timeout")
	}

	m.AssertExpectations(t)
}

func TestListener_ContextCancel(t *testing.T) {
	m := &runtime.MockRuntime{}
	_, _ = setupMockStream(m)

	logger := slog.Default()
	l := NewListenerWithDebounce(m, logger, testDebounce)
	ctx, cancel := context.WithCancel(context.Background())

	triggerCh := l.Listen(ctx)

	// Cancel context.
	cancel()

	// Channel should close.
	select {
	case _, ok := <-triggerCh:
		assert.False(t, ok, "trigger channel should be closed after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("expected trigger channel to close after context cancel")
	}
}

func TestListener_ErrorReconnect(t *testing.T) {
	m := &runtime.MockRuntime{}

	// First stream: will send an error.
	eventCh1 := make(chan runtime.Event)
	errCh1 := make(chan error, 1)
	m.On("EventStream", mock.Anything).Return((<-chan runtime.Event)(eventCh1), (<-chan error)(errCh1)).Once()

	// Second stream: reconnected after error.
	eventCh2 := make(chan runtime.Event)
	errCh2 := make(chan error)
	m.On("EventStream", mock.Anything).Return((<-chan runtime.Event)(eventCh2), (<-chan error)(errCh2)).Once()

	logger := slog.Default()
	l := NewListenerWithDebounce(m, logger, testDebounce)
	l.backoffDuration = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	triggerCh := l.Listen(ctx)

	// Send error to trigger reconnect.
	errCh1 <- assert.AnError

	// Give time for reconnect.
	time.Sleep(200 * time.Millisecond)

	// Send event on second stream.
	eventCh2 <- runtime.Event{Type: "volume", Action: "create", Actor: "after-error"}

	// Should receive trigger.
	select {
	case <-triggerCh:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("expected trigger after error reconnect, got timeout")
	}

	m.AssertExpectations(t)
}
