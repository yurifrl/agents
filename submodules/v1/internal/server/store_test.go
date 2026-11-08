package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTaskStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := store.Create(protocol.CreateTaskInput{
		Owner: "o1", Name: "n1", Task: "t1", Cwd: "/tmp", Model: "m1",
	}, protocol.SurfaceProcess)
	if _, err := store.AppendEvent(created.ID, protocol.EventLog, protocol.TaskEventData{Line: new("hello")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.SaveNow(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reopened, err := NewTaskStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Get(created.ID)
	if got == nil {
		t.Fatal("task lost after reopen")
	}
	if got.Model != "m1" || got.Status != protocol.StatusQueued || got.Acknowledged {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	events := reopened.EventsSince(created.ID, 0)
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("events after reopen = %+v", events)
	}
	// Seq continues after reload.
	next, err := reopened.AppendEvent(created.ID, protocol.EventLog, protocol.TaskEventData{Line: new("again")})
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if next.Seq != 2 {
		t.Fatalf("seq = %d, want 2", next.Seq)
	}
}

func TestStoreFiltersAndSorting(t *testing.T) {
	store, err := NewTaskStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a := store.Create(protocol.CreateTaskInput{Owner: "alice", Name: "a", Task: "t", Cwd: "/tmp"}, protocol.SurfaceProcess)
	time.Sleep(2 * time.Millisecond) // createdAt ordering must be deterministic
	b := store.Create(protocol.CreateTaskInput{Owner: "bob", Name: "b", Task: "t", Cwd: "/tmp"}, protocol.SurfaceProcess)
	time.Sleep(2 * time.Millisecond)
	c := store.Create(protocol.CreateTaskInput{Owner: "alice", Name: "c", Task: "t", Cwd: "/tmp"}, protocol.SurfaceProcess)
	if _, err := store.Ack(c.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	all := store.List(TaskFilter{})
	if len(all) != 3 || all[0].ID != a.ID || all[1].ID != b.ID || all[2].ID != c.ID {
		t.Fatalf("all = %d entries, order wrong", len(all))
	}
	alice := store.List(TaskFilter{Owner: "alice"})
	if len(alice) != 2 {
		t.Fatalf("alice = %d", len(alice))
	}
	unacked := store.List(TaskFilter{Owner: "alice", Unacknowledged: true})
	if len(unacked) != 1 || unacked[0].ID != a.ID {
		t.Fatalf("unacked = %+v", unacked)
	}
	// Get returns copies; mutating them must not affect the store.
	mutated := store.Get(a.ID)
	mutated.Status = protocol.StatusCompleted
	if fresh := store.Get(a.ID); fresh.Status != protocol.StatusQueued {
		t.Fatal("Get leaked internal state")
	}
}

func TestStoreUpdatePatch(t *testing.T) {
	store, err := NewTaskStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := store.Create(protocol.CreateTaskInput{Owner: "o", Name: "n", Task: "t", Cwd: "/tmp"}, protocol.SurfaceHerdr)
	now := int64(1234)
	updated, err := store.Update(created.ID, TaskPatch{
		Status:    new(protocol.StatusRunning),
		StartedAt: new(now),
		Worker:    &protocol.TaskWorkerMeta{PaneID: "w1:p1", Port: new(4321)},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != protocol.StatusRunning || *updated.StartedAt != now || updated.Worker.PaneID != "w1:p1" || *updated.Worker.Port != 4321 {
		t.Fatalf("patch not applied: %+v", updated)
	}
	if _, err := store.Update("missing", TaskPatch{}); err == nil {
		t.Fatal("update unknown must fail")
	}
}

func TestStoreEventLogPersisted(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTaskStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := store.Create(protocol.CreateTaskInput{Owner: "o", Name: "n", Task: "t", Cwd: "/tmp"}, protocol.SurfaceProcess)
	for range 15 {
		if _, err := store.AppendEvent(created.ID, protocol.EventActivity, protocol.TaskEventData{ToolName: "tool", Detail: "x"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// File on disk contains 15 JSONL lines with seq 1..15.
	data, err := os.ReadFile(filepath.Join(dir, protocol.EventsDirname, created.ID+".jsonl"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var seqs []int64
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var e protocol.TaskEvent
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 15 || seqs[0] != 1 || seqs[14] != 15 {
		t.Fatalf("seqs = %v", seqs)
	}
	recent := store.RecentEvents(created.ID, 10)
	if len(recent) != 10 || recent[0].Seq != 6 {
		t.Fatalf("recent = %d entries starting seq %d", len(recent), recent[0].Seq)
	}
	// Empty-text events serialize with the key present (variant fidelity).
	ev, err := store.AppendEvent(created.ID, protocol.EventResult, protocol.TaskEventData{Text: new("")})
	if err != nil {
		t.Fatalf("append result: %v", err)
	}
	line, _ := json.Marshal(ev.Data)
	if string(line) != `{"text":""}` {
		t.Fatalf("result data = %s, want {\"text\":\"\"}", line)
	}
}

func TestStoreSubscribe(t *testing.T) {
	store, err := NewTaskStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := store.Create(protocol.CreateTaskInput{Owner: "o", Name: "n", Task: "t", Cwd: "/tmp"}, protocol.SurfaceProcess)

	got := make(chan protocol.TaskEvent, 4)
	cancel := store.Subscribe(created.ID, func(e protocol.TaskEvent) { got <- e })
	if _, err := store.AppendEvent(created.ID, protocol.EventLog, protocol.TaskEventData{Line: new("one")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if e := <-got; *e.Data.Line != "one" {
		t.Fatalf("event = %+v", e)
	}
	cancel()
	if _, err := store.AppendEvent(created.ID, protocol.EventLog, protocol.TaskEventData{Line: new("two")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	select {
	case e := <-got:
		t.Fatalf("event after unsubscribe: %+v", e)
	default:
	}
}
