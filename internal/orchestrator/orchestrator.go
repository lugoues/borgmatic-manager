package orchestrator

import (
	"context"
	"log/slog"
)

// CycleRunner abstracts the scheduler's backup cycle operations.
type CycleRunner interface {
	RunCycle(ctx context.Context) error
	Start(ctx context.Context) error
}

// EventSource abstracts the event listener's trigger channel.
type EventSource interface {
	Listen(ctx context.Context) <-chan struct{}
}

// Orchestrator coordinates the scheduler and event listener into one main loop.
type Orchestrator struct {
	scheduler CycleRunner
	listener  EventSource
	logger    *slog.Logger
}

// NewOrchestrator creates an Orchestrator.
func NewOrchestrator(scheduler CycleRunner, listener EventSource, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		scheduler: scheduler,
		listener:  listener,
		logger:    logger,
	}
}

// Run performs an initial cycle, then loops: the scheduler ticker runs in a
// goroutine while events trigger extra cycles. Returns nil on clean shutdown.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.logger.Info("borgmatic-manager starting up")

	// Subscribe before the initial cycle: a container created while that cycle
	// runs parks a trigger in the buffered channel instead of being missed.
	triggerCh := o.listener.Listen(ctx)

	if err := o.scheduler.RunCycle(ctx); err != nil {
		o.logger.Error("initial cycle failed", "error", err)
	}

	// Shutdown must wait for the scheduler goroutine: abandoning a mid-backup
	// cycle would cut borgmatic off instead of the clean SIGTERM-and-wait.
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		if err := o.scheduler.Start(ctx); err != nil {
			o.logger.Error("scheduler stopped with error", "error", err)
		}
	}()
	joinScheduler := func() {
		o.logger.Info("waiting for in-flight cycle to finish")
		<-schedulerDone
	}

	for {
		select {
		case _, ok := <-triggerCh:
			if !ok {
				o.logger.Info("event listener stopped")
				joinScheduler()
				return nil
			}
			o.logger.Info("re-discovery triggered by event")
			if err := o.scheduler.RunCycle(ctx); err != nil {
				o.logger.Error("event-triggered cycle failed", "error", err)
			}
		case <-ctx.Done():
			o.logger.Info("shutting down")
			joinScheduler()
			return nil
		}
	}
}
