package main

import (
	"context"
	"log/slog"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const vcsMigrationPollInterval = 2 * time.Second

func vcsMigrationReady(ctx context.Context, queries *db.Queries) bool {
	if queries == nil {
		return false
	}
	state, err := queries.GetVCSGitLabMigrationState(ctx)
	if err != nil {
		return false
	}
	return state.Phase == "complete" && !state.MaintenanceMode
}

// runVCSMigrationGatedWorker starts a worker only while GitLab's data cutover is
// finalized, waits for cancellation to drain it, and restarts it after the
// migration becomes ready again.
func runVCSMigrationGatedWorker(ctx context.Context, queries *db.Queries, run func(context.Context)) {
	runVCSMigrationGatedWorkerWithReadiness(ctx, vcsMigrationPollInterval,
		func(ctx context.Context) bool { return vcsMigrationReady(ctx, queries) },
		run,
	)
}

func runVCSMigrationGatedWorkerWithReadiness(
	ctx context.Context,
	pollInterval time.Duration,
	ready func(context.Context) bool,
	run func(context.Context),
) {
	if pollInterval <= 0 {
		pollInterval = vcsMigrationPollInterval
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		var workerCancel context.CancelFunc
		var workerDone <-chan struct{}
		stopWorker := func() {
			if workerDone == nil {
				return
			}
			workerCancel()
			<-workerDone
			workerCancel = nil
			workerDone = nil
		}
		startWorker := func() {
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workerCancel = cancel
			workerDone = done
			go func() {
				defer close(done)
				run(workerCtx)
			}()
		}
		defer stopWorker()

		if ctx.Err() == nil && ready(ctx) {
			startWorker()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-workerDone:
				workerCancel()
				workerCancel = nil
				workerDone = nil
			case <-ticker.C:
				isReady := ctx.Err() == nil && ready(ctx)
				if workerDone != nil && !isReady {
					stopWorker()
				}
				if workerDone == nil && isReady {
					startWorker()
				}
			}
		}
	}()
}

// runVCSMigrationGatedWorkerOnce is for components whose lifecycle is
// intentionally single-start. It waits through maintenance, starts once when
// ready, and cancels permanently if an unsupported live transition re-enters
// maintenance; a later completion requires a process restart.
func runVCSMigrationGatedWorkerOnce(ctx context.Context, queries *db.Queries, run func(context.Context)) {
	runVCSMigrationGatedWorkerOnceWithReadiness(ctx, vcsMigrationPollInterval,
		func(ctx context.Context) bool { return vcsMigrationReady(ctx, queries) },
		run,
	)
}

func runVCSMigrationGatedWorkerOnceWithReadiness(
	ctx context.Context,
	pollInterval time.Duration,
	ready func(context.Context) bool,
	run func(context.Context),
) {
	if pollInterval <= 0 {
		pollInterval = vcsMigrationPollInterval
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for ctx.Err() == nil && !ready(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		if ctx.Err() != nil {
			return
		}

		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			run(workerCtx)
		}()
		defer func() {
			cancel()
			<-done
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if !ready(ctx) {
					slog.Error("single-start VCS-gated worker stopped after migration gate closed; restart the process after completion")
					return
				}
			}
		}
	}()
}

// runVCSMigrationStartOnce waits for one ready state before starting a
// non-reentrant component. Its lifetime is bound to ctx; later gate changes are
// handled by that component's own safe pause mechanism or require a process
// restart.
func runVCSMigrationStartOnce(ctx context.Context, queries *db.Queries, start func()) {
	runVCSMigrationStartOnceWithReadiness(ctx, vcsMigrationPollInterval,
		func(ctx context.Context) bool { return vcsMigrationReady(ctx, queries) },
		start,
	)
}

func runVCSMigrationStartOnceWithReadiness(
	ctx context.Context,
	pollInterval time.Duration,
	ready func(context.Context) bool,
	start func(),
) {
	if pollInterval <= 0 {
		pollInterval = vcsMigrationPollInterval
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for ctx.Err() == nil && !ready(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		if ctx.Err() == nil {
			start()
		}
	}()
}

// watchVCSMigrationGate reports the initial readiness and subsequent changes.
// It deliberately does not cancel a started one-shot worker.
func watchVCSMigrationGate(ctx context.Context, queries *db.Queries, onChange func(bool)) {
	watchVCSMigrationGateWithReadiness(ctx, vcsMigrationPollInterval,
		func(ctx context.Context) bool { return vcsMigrationReady(ctx, queries) },
		onChange,
	)
}

func watchVCSMigrationGateWithReadiness(
	ctx context.Context,
	pollInterval time.Duration,
	ready func(context.Context) bool,
	onChange func(bool),
) {
	if pollInterval <= 0 {
		pollInterval = vcsMigrationPollInterval
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		observed, last := false, false
		for ctx.Err() == nil {
			current := ready(ctx)
			if !observed || current != last {
				onChange(current)
				observed, last = true, current
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
