package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/protocol"
)

// newTestApp wires a full app (store+supervisor+handler) over a scripted
// fixture surface.
func newTestApp(t *testing.T, mode ompFixtureMode) (*App, string) {
	t.Helper()
	sup, _, _ := newTestSupervisor(t, mode, nil)
	config := Config{Port: 0, StateDir: t.TempDir(), Surface: "process", OmpBin: "omp", MaxTimeS: 1800, URL: "http://127.0.0.1:0"}
	handler := NewHTTPHandler(config, sup.store, sup, 20*time.Millisecond)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	app := &App{Config: config, Store: sup.store, Supervisor: sup}
	return app, ts.URL
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestHTTPCreateValidation(t *testing.T) {
	_, base := newTestApp(t, fixtureSettled)

	resp := post(t, base+"/tasks", map[string]any{"owner": "o", "name": "n", "task": "t", "cwd": "relative/path"})
	var errBody map[string]string
	decode(t, resp, &errBody)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errBody["error"], "absolute") {
		t.Fatalf("relative cwd: %d %v", resp.StatusCode, errBody)
	}

	resp = post(t, base+"/tasks", map[string]any{"owner": "", "name": "n", "task": "t", "cwd": "/tmp"})
	decode(t, resp, &errBody)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errBody["error"], "owner") {
		t.Fatalf("empty owner: %d %v", resp.StatusCode, errBody)
	}
}

func TestHTTPCreateListGetAckFlow(t *testing.T) {
	_, base := newTestApp(t, fixtureSettled)

	resp := post(t, base+"/tasks", map[string]any{
		"owner": "parent-1", "name": "demo", "task": "work", "cwd": "/tmp",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created protocol.Task
	decode(t, resp, &created)
	if created.Status != protocol.StatusQueued || created.Surface != protocol.SurfaceProcess {
		t.Fatalf("created = %+v", created)
	}

	// Wait for completion through the API.
	deadline := time.Now().Add(5 * time.Second)
	var detail protocol.Task
	for {
		resp, err := http.Get(base + protocol.PathTask(created.ID))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		decode(t, resp, &detail)
		if detail.Status == protocol.StatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task stuck at %s (%s)", detail.Status, detail.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if detail.Result == nil || *detail.Result != "final answer text" {
		t.Fatalf("result = %v", detail.Result)
	}
	if len(detail.RecentEvents) == 0 {
		t.Fatal("detail must embed recentEvents")
	}

	// List filters.
	resp, _ = http.Get(base + "/tasks?owner=parent-1&unacknowledged=true")
	var listed []protocol.Task
	decode(t, resp, &listed)
	if len(listed) != 1 {
		t.Fatalf("listed = %d", len(listed))
	}

	// Ack removes from unacknowledged attach results.
	resp = post(t, base+protocol.PathAck(created.ID), map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack = %d", resp.StatusCode)
	}
	resp = post(t, base+"/attach", map[string]any{"owner": "parent-1"})
	var attach protocol.AttachResponse
	decode(t, resp, &attach)
	if len(attach.Tasks) != 0 || attach.Server.Surface != protocol.SurfaceProcess || attach.Server.Version == "" {
		t.Fatalf("attach = %+v", attach)
	}
}

func TestHTTPHealth(t *testing.T) {
	_, base := newTestApp(t, fixtureSettled)
	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var health protocol.HealthResponse
	decode(t, resp, &health)
	if !health.Ok || health.Version == "" || health.Surface != protocol.SurfaceProcess {
		t.Fatalf("health = %+v", health)
	}
}

func TestHTTPSteerAndNotFound(t *testing.T) {
	_, base := newTestApp(t, fixtureBusy)
	resp := post(t, base+"/tasks", map[string]any{"owner": "o", "name": "n", "task": "work", "cwd": "/tmp"})
	var created protocol.Task
	decode(t, resp, &created)

	// Wait until running, then steer.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := http.Get(base + protocol.PathTask(created.ID))
		var cur protocol.Task
		decode(t, resp, &cur)
		if cur.Status == protocol.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck at %s", cur.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp = post(t, base+protocol.PathSteer(created.ID), map[string]any{"message": "nudge"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("steer = %d", resp.StatusCode)
	}

	resp = post(t, base+protocol.PathSteer(created.ID), map[string]any{"message": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty steer = %d", resp.StatusCode)
	}

	resp, _ = http.Get(base + protocol.PathTask("does-not-exist"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task = %d", resp.StatusCode)
	}

	resp, _ = http.Get(base + "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route = %d", resp.StatusCode)
	}

	_, _ = http.Post(base+protocol.PathCancel(created.ID), "application/json", strings.NewReader("{}"))
}

func TestHTTPStreamReplayAndClose(t *testing.T) {
	_, base := newTestApp(t, fixtureSettled)
	resp := post(t, base+"/tasks", map[string]any{"owner": "o", "name": "n", "task": "work", "cwd": "/tmp"})
	var created protocol.Task
	decode(t, resp, &created)

	// Attach to the stream before the worker finishes to catch live events.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+protocol.PathStream(created.ID)+"?since=0", nil)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer httpResp.Body.Close()
	if ct := httpResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %s", ct)
	}

	scanner := bufio.NewScanner(httpResp.Body)
	terminal := false
	deadline := time.After(5 * time.Second)
	for !terminal {
		got := make(chan string)
		go func() {
			for scanner.Scan() {
				got <- scanner.Text()
				return
			}
			close(got)
		}()
		select {
		case line, ok := <-got:
			if !ok {
				t.Fatal("stream closed before terminal event")
			}
			if strings.HasPrefix(line, "data: ") {
				var frame protocol.SseFrame
				if err := json.Unmarshal([]byte(line[len("data: "):]), &frame); err != nil {
					t.Fatalf("bad sse frame %q: %v", line, err)
				}
				if frame.Type == protocol.EventStateChanged && protocol.IsTerminalStatus(frame.Data.To) {
					terminal = true
				}
			}
		case <-deadline:
			t.Fatal("no terminal event within budget")
		}
	}

	// Replaying from 0 after completion returns the whole log and closes.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+protocol.PathStreamSince(created.ID, 0), nil)
	replay, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer replay.Body.Close()
	scanner2 := bufio.NewScanner(replay.Body)
	frames := 0
	sawTerminal := false
	for scanner2.Scan() {
		line := scanner2.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		frames++
		var frame protocol.SseFrame
		_ = json.Unmarshal([]byte(line[len("data: "):]), &frame)
		if frame.Type == protocol.EventStateChanged && protocol.IsTerminalStatus(frame.Data.To) {
			sawTerminal = true
		}
	}
	if !sawTerminal || frames == 0 {
		t.Fatalf("replay frames=%d terminal=%v", frames, sawTerminal)
	}
}

func TestHTTPStreamNotFoundAndBadSince(t *testing.T) {
	_, base := newTestApp(t, fixtureSettled)
	resp, _ := http.Get(base + protocol.PathStream("missing"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing stream = %d", resp.StatusCode)
	}
	resp, _ = http.Get(base + protocol.PathStreamSince("missing", -3))
	_ = resp
	// A real task id with a negative since is a 400.
	created := post(t, base+"/tasks", map[string]any{"owner": "o", "name": "n", "task": "work", "cwd": "/tmp"})
	var task protocol.Task
	decode(t, created, &task)
	resp, _ = http.Get(base + fmt.Sprintf("/tasks/%s/stream?since=-1", task.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since = %d", resp.StatusCode)
	}
}
