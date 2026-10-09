package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
	"github.com/yurifrl/agents/internal/surface"
)

// fakeSurface delegates to the real ProcessSurface but records launch specs
// and allows injecting a different ompBin (the scripted fixture).
type fakeSurface struct {
	inner   surface.Surface
	started []surface.WorkerSpec
	mu      sync.Mutex
}

func (f *fakeSurface) Kind() protocol.SurfaceKind { return f.inner.Kind() }

func (f *fakeSurface) Start(spec surface.WorkerSpec) (surface.RunningWorker, error) {
	f.mu.Lock()
	f.started = append(f.started, spec)
	f.mu.Unlock()
	return f.inner.Start(spec)
}

func newTestSupervisor(t *testing.T, mode ompFixtureMode, mutate func(*SupervisorOpts)) (*Supervisor, *fakeSurface, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewTaskStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Flush the debounced store save and stop background work before t.TempDir
	// removes the directory; otherwise a pending save races RemoveAll.
	var sup *Supervisor
	t.Cleanup(func() { sup.Shutdown() })
	ompBin := writeOmpFixture(t, dir, mode)
	surf := &fakeSurface{inner: surface.NewProcessSurface()}
	opts := SupervisorOpts{
		OmpBin:      ompBin,
		MaxTime:     5 * time.Second,
		SettleQuiet: 150 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	sup = NewSupervisor(store, surf, opts)
	return sup, surf, dir
}

func waitForStatus(t *testing.T, sup *Supervisor, id string, want protocol.TaskStatus) protocol.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if task := sup.store.Get(id); task != nil && task.Status == want {
			return *task
		}
		if time.Now().After(deadline) {
			task := sup.store.Get(id)
			status, errMsg := "", ""
			if task != nil {
				status, errMsg = string(task.Status), task.Error
			}
			t.Fatalf("task %s never reached %s (status=%s error=%q)", id, want, status, errMsg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupervisorCompletesTask(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureSettled, nil)
	task := sup.Create(protocol.CreateTaskInput{
		Owner: "p1", Name: "demo", Task: "do the thing", Cwd: dir,
	})
	final := waitForStatus(t, sup, task.ID, protocol.StatusCompleted)

	if final.Result == nil || *final.Result != "final answer text" {
		t.Fatalf("result = %v, want final answer text", final.Result)
	}
	if final.SessionFile == "" {
		t.Fatal("sessionFile not recorded from ready frame")
	}
	if final.StartedAt == nil || final.EndedAt == nil {
		t.Fatal("startedAt/endedAt not set")
	}
	if final.Acknowledged {
		t.Fatal("fresh task must not be acknowledged")
	}
	if final.Worker == nil || final.Worker.PID == nil {
		t.Fatal("worker meta missing")
	}
	// Event log carries the full trajectory.
	events := sup.store.EventsSince(task.ID, 0)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.Type != protocol.EventStateChanged || last.Data.To != protocol.StatusCompleted {
		t.Fatalf("terminal event = %+v", last.Data)
	}
	var sawActivity, sawResult bool
	for _, e := range events {
		if e.Type == protocol.EventActivity && e.Data.ToolName == "read_file" {
			sawActivity = true
			if e.Data.Detail == "" || !strings.Contains(e.Data.Detail, "path") {
				t.Fatalf("activity detail = %q", e.Data.Detail)
			}
		}
		if e.Type == protocol.EventResult {
			sawResult = true
			if e.Data.Text == nil || *e.Data.Text != "final answer text" {
				t.Fatalf("result event = %+v", e.Data)
			}
		}
	}
	if !sawActivity || !sawResult {
		t.Fatalf("missing events (activity=%v result=%v)", sawActivity, sawResult)
	}
}

func TestSupervisorSettleQuietFallback(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureQuiet, nil)
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "quiet", Task: "work", Cwd: dir})
	final := waitForStatus(t, sup, task.ID, protocol.StatusCompleted)
	if final.Result == nil || *final.Result != "final answer text" {
		t.Fatalf("quiet-settle result = %v", final.Result)
	}
}

func TestSupervisorWorkerCrashFails(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureCrash, nil)
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "boom", Task: "work", Cwd: dir})
	final := waitForStatus(t, sup, task.ID, protocol.StatusFailed)
	if final.Error == "" {
		t.Fatal("failed task must carry an error message")
	}
}

func TestSupervisorDeadlineExceeded(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureHang, func(o *SupervisorOpts) {
		o.MaxTime = 150 * time.Millisecond
	})
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "slow", Task: "work", Cwd: dir})
	final := waitForStatus(t, sup, task.ID, protocol.StatusFailed)
	if !strings.Contains(final.Error, "deadline exceeded") {
		t.Fatalf("error = %q", final.Error)
	}
}

func TestSupervisorSteerAndCancel(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureSteer, func(o *SupervisorOpts) {
		o.SettleQuiet = 800 * time.Millisecond // give the cancel room to win
	})
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "steerable", Task: "work", Cwd: dir})

	// Steer before running: buffered, flushed after the prompt lands.
	if _, err := sup.Steer(context.Background(), task.ID, "early nudge"); err != nil {
		t.Fatalf("queued steer: %v", err)
	}
	// Empty steer rejected.
	if _, err := sup.Steer(context.Background(), task.ID, "  "); err == nil {
		t.Fatal("empty steer must be rejected")
	}
	waitForStatus(t, sup, task.ID, protocol.StatusRunning)

	// Steer while running: accepted, logged.
	if _, err := sup.Steer(context.Background(), task.ID, "mid-flight nudge"); err != nil {
		t.Fatalf("running steer: %v", err)
	}
	var sawSteer bool
	for _, e := range sup.store.EventsSince(task.ID, 0) {
		if e.Type == protocol.EventLog && e.Data.Line != nil && strings.Contains(*e.Data.Line, "steered: mid-flight nudge") {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatal("steer not logged")
	}

	// Cancel mid-flight wins the race against the quiet-window settle.
	if _, err := sup.Cancel(context.Background(), task.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	final := waitForStatus(t, sup, task.ID, protocol.StatusCanceled)
	if !final.StopRequested {
		t.Fatal("stopRequested must be set on cancel")
	}
}

func TestSupervisorCancelQueued(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureSettled, nil)
	blocker := &blockingSurface{inner: surface.NewProcessSurface(), releaseC: make(chan struct{})}
	sup.surface = blocker
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "queued", Task: "work", Cwd: dir})
	if _, err := sup.Cancel(context.Background(), task.ID); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	final := waitForStatus(t, sup, task.ID, protocol.StatusCanceled)
	// TS parity: transition() stamps startedAt on the first edge out of
	// queued (queued→canceled included); the worker, however, never launched.
	if final.StartedAt == nil {
		t.Fatal("canceled task must carry startedAt")
	}
	if final.Worker != nil {
		t.Fatal("queued cancel must not record worker meta")
	}
	blocker.release()
}

type blockingSurface struct {
	inner     surface.Surface
	releaseC  chan struct{}
	once      sync.Once
	released  bool
	releaseMu sync.Mutex
}

func (b *blockingSurface) Kind() protocol.SurfaceKind { return b.inner.Kind() }

func (b *blockingSurface) Start(spec surface.WorkerSpec) (surface.RunningWorker, error) {
	b.releaseMu.Lock()
	released := b.released
	b.releaseMu.Unlock()
	if !released {
		<-b.releaseC
	}
	return b.inner.Start(spec)
}

func (b *blockingSurface) release() {
	b.releaseMu.Lock()
	b.released = true
	b.releaseMu.Unlock()
	b.once.Do(func() { close(b.releaseC) })
}

func TestSupervisorAck(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureSettled, nil)
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "ack", Task: "work", Cwd: dir})
	waitForStatus(t, sup, task.ID, protocol.StatusCompleted)
	got, err := sup.Ack(task.ID)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if !got.Acknowledged {
		t.Fatal("acknowledged not set")
	}
	if _, err := sup.Ack("nope"); err == nil {
		t.Fatal("ack unknown task must fail")
	}
}

func TestSupervisorWhenSettled(t *testing.T) {
	sup, _, dir := newTestSupervisor(t, fixtureSettled, nil)
	task := sup.Create(protocol.CreateTaskInput{Owner: "p1", Name: "settled", Task: "work", Cwd: dir})
	select {
	case final := <-sup.WhenSettled(task.ID):
		if final == nil || final.Status != protocol.StatusCompleted {
			t.Fatalf("settled = %+v", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("whenSettled never resolved")
	}
}
