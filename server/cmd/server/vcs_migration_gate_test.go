package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunVCSMigrationGatedWorkerCancelsDrainsAndResumes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ready atomic.Bool
	started := make(chan int32, 2)
	cancelled := make(chan int32, 2)
	finished := make(chan int32, 2)
	releaseFirst := make(chan struct{})
	var runs atomic.Int32

	runVCSMigrationGatedWorkerWithReadiness(ctx, 5*time.Millisecond,
		func(context.Context) bool { return ready.Load() },
		func(ctx context.Context) {
			id := runs.Add(1)
			started <- id
			<-ctx.Done()
			cancelled <- id
			if id == 1 {
				<-releaseFirst
			}
			finished <- id
		})

	select {
	case id := <-started:
		t.Fatalf("worker started while maintenance was active: run %d", id)
	case <-time.After(30 * time.Millisecond):
	}

	ready.Store(true)
	if id := awaitWorkerSignal(t, started); id != 1 {
		t.Fatalf("first run id = %d, want 1", id)
	}
	ready.Store(false)
	if id := awaitWorkerSignal(t, cancelled); id != 1 {
		t.Fatalf("cancelled run id = %d, want 1", id)
	}
	select {
	case id := <-started:
		t.Fatalf("worker restarted before the cancelled run drained: run %d", id)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	if id := awaitWorkerSignal(t, finished); id != 1 {
		t.Fatalf("finished run id = %d, want 1", id)
	}

	ready.Store(true)
	if id := awaitWorkerSignal(t, started); id != 2 {
		t.Fatalf("resumed run id = %d, want 2", id)
	}
	cancel()
	if id := awaitWorkerSignal(t, cancelled); id != 2 {
		t.Fatalf("shutdown cancelled run id = %d, want 2", id)
	}
	if id := awaitWorkerSignal(t, finished); id != 2 {
		t.Fatalf("shutdown finished run id = %d, want 2", id)
	}
}

func TestRunVCSMigrationGatedWorkerOnceStopsWithoutRestarting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ready atomic.Bool
	var runs atomic.Int32
	started := make(chan int32, 2)
	cancelled := make(chan int32, 2)
	runVCSMigrationGatedWorkerOnceWithReadiness(ctx, 5*time.Millisecond,
		func(context.Context) bool { return ready.Load() },
		func(workerCtx context.Context) {
			started <- runs.Add(1)
			<-workerCtx.Done()
			cancelled <- runs.Load()
		})

	select {
	case id := <-started:
		t.Fatalf("one-shot worker started during maintenance: run %d", id)
	case <-time.After(30 * time.Millisecond):
	}
	ready.Store(true)
	if id := awaitWorkerSignal(t, started); id != 1 {
		t.Fatalf("one-shot run id = %d, want 1", id)
	}
	ready.Store(false)
	if id := awaitWorkerSignal(t, cancelled); id != 1 {
		t.Fatalf("one-shot cancelled id = %d, want 1", id)
	}
	ready.Store(true)
	select {
	case id := <-started:
		t.Fatalf("non-reentrant worker restarted in the same process: run %d", id)
	case <-time.After(30 * time.Millisecond):
	}
}

func awaitWorkerSignal(t *testing.T, ch <-chan int32) int32 {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gated worker transition")
		return 0
	}
}

func TestRunVCSMigrationStartOnceWaitsAndStartsOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	var starts atomic.Int32
	started := make(chan int32, 2)
	runVCSMigrationStartOnceWithReadiness(ctx, 5*time.Millisecond,
		func(context.Context) bool { return ready.Load() },
		func() { started <- starts.Add(1) },
	)

	select {
	case id := <-started:
		t.Fatalf("start-once component started during maintenance: %d", id)
	case <-time.After(30 * time.Millisecond):
	}
	ready.Store(true)
	if id := awaitWorkerSignal(t, started); id != 1 {
		t.Fatalf("start-once invocation = %d, want 1", id)
	}
	ready.Store(false)
	time.Sleep(20 * time.Millisecond)
	ready.Store(true)
	select {
	case id := <-started:
		t.Fatalf("start-once component started again: %d", id)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestWatchVCSMigrationGateReportsInitialStateAndResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	changes := make(chan bool, 4)
	watchVCSMigrationGateWithReadiness(ctx, 5*time.Millisecond,
		func(context.Context) bool { return ready.Load() },
		func(value bool) { changes <- value },
	)
	awaitGateChange(t, changes, false)
	ready.Store(true)
	awaitGateChange(t, changes, true)
	ready.Store(false)
	awaitGateChange(t, changes, false)
	ready.Store(true)
	awaitGateChange(t, changes, true)
}

func awaitGateChange(t *testing.T, changes <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-changes:
		if got != want {
			t.Fatalf("migration gate change = %t, want %t", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for migration gate change to %t", want)
	}
}
