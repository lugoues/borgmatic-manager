package orchestrator

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// mockCycleRunner tracks calls to RunCycle and Start.
type mockCycleRunner struct {
	runCycleCount atomic.Int32
	startCalled   atomic.Bool
	startBlock    chan struct{} // blocks Start until closed
}

func newMockCycleRunner() *mockCycleRunner {
	return &mockCycleRunner{
		startBlock: make(chan struct{}),
	}
}

func (m *mockCycleRunner) RunCycle(ctx context.Context) error {
	m.runCycleCount.Add(1)
	return nil
}

func (m *mockCycleRunner) Start(ctx context.Context) error {
	m.startCalled.Store(true)
	<-m.startBlock // block until released
	return nil
}

// mockEventSource returns a trigger channel controlled by the test.
type mockEventSource struct {
	triggerCh chan struct{}
}

func newMockEventSource() *mockEventSource {
	return &mockEventSource{
		triggerCh: make(chan struct{}, 1),
	}
}

func (m *mockEventSource) Listen(ctx context.Context) <-chan struct{} {
	return m.triggerCh
}

func TestOrchestrator_StartupSequence(t *testing.T) {
	runner := newMockCycleRunner()
	events := newMockEventSource()
	logger := slog.Default()

	o := NewOrchestrator(runner, events, logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Let Start return immediately so Run doesn't hang forever.
	close(runner.startBlock)

	// Cancel after a short delay to allow startup sequence to complete.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := o.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error on clean shutdown, got: %v", err)
	}

	// RunCycle should have been called at least once for initial startup.
	if runner.runCycleCount.Load() < 1 {
		t.Error("expected RunCycle to be called at least once during startup")
	}

	// Start should have been called for the ticker goroutine.
	if !runner.startCalled.Load() {
		t.Error("expected Start to be called for scheduler ticker")
	}
}

func TestOrchestrator_Shutdown(t *testing.T) {
	runner := newMockCycleRunner()
	events := newMockEventSource()
	logger := slog.Default()

	o := NewOrchestrator(runner, events, logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately after startup.
	close(runner.startBlock)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := o.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error on clean shutdown, got: %v", err)
	}
}

func TestOrchestrator_EventTrigger(t *testing.T) {
	runner := newMockCycleRunner()
	events := newMockEventSource()
	logger := slog.Default()

	o := NewOrchestrator(runner, events, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Let Start block (simulates real ticker running in background).
	// We'll cancel context to end the test.

	go func() {
		// Wait for initial RunCycle to complete.
		for runner.runCycleCount.Load() < 1 {
			time.Sleep(5 * time.Millisecond)
		}

		// Send event trigger.
		events.triggerCh <- struct{}{}

		// Wait for event-triggered RunCycle.
		deadline := time.After(2 * time.Second)
		for runner.runCycleCount.Load() < 2 {
			select {
			case <-deadline:
				t.Error("timed out waiting for event-triggered RunCycle")
				cancel()
				return
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}

		// Shutdown.
		cancel()
	}()

	err := o.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error on clean shutdown, got: %v", err)
	}

	// Should have at least 2 RunCycle calls: 1 startup + 1 event-triggered.
	if runner.runCycleCount.Load() < 2 {
		t.Errorf("expected at least 2 RunCycle calls, got %d", runner.runCycleCount.Load())
	}
}
