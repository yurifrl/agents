package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yurifrl/agents/internal/a2a"
	"github.com/yurifrl/agents/internal/agent"
	"github.com/yurifrl/agents/internal/agentmd"
	"github.com/yurifrl/agents/internal/output"
	"github.com/yurifrl/agents/internal/tool"
)

type stubTools struct {
	calls []tool.Call
}

func (s *stubTools) Schemas() []json.RawMessage {
	return []json.RawMessage{json.RawMessage(`{"name":"exec","description":"run"}`)}
}

func (s *stubTools) Execute(_ context.Context, c tool.Call) tool.Result {
	s.calls = append(s.calls, c)
	return tool.Result{Text: "tool output"}
}

func anthropicServer(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i >= len(bodies) {
			t.Errorf("unexpected extra request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(bodies[i]))
		i++
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testDefinition() *agentmd.Definition {
	return &agentmd.Definition{
		Name:        "tester",
		Model:       "claude-test",
		Output:      "response",
		Instruction: "you are a test agent",
	}
}

func TestRunResponseWithToolsEmitsToolEvents(t *testing.T) {
	srv := anthropicServer(t,
		`{"content":[{"type":"tool_use","id":"toolu_1","name":"exec","input":{"argv":["ls"]}}],"stop_reason":"tool_use"}`,
		`{"content":[{"type":"text","text":"final answer"}],"stop_reason":"end_turn"}`,
	)
	client := agent.NewClient("k", agent.WithBaseURL(srv.URL))
	tools := &stubTools{}
	r := New(client, testDefinition(), "", output.Author{})
	r.SetTools(tools)

	var events []a2a.Event
	result, err := r.Run(context.Background(), a2a.InvokeRequest{Prompt: "do it"}, func(ev a2a.Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "final answer" {
		t.Fatalf("got %q", result.Output)
	}

	var types []a2a.EventType
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := []a2a.EventType{a2a.EventToolCall, a2a.EventToolResult, a2a.EventArtifact}
	if len(types) != len(want) {
		t.Fatalf("want events %v, got %v", want, types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("want events %v, got %v", want, types)
		}
	}
	if events[0].Tool == nil || events[0].Tool.Name != "exec" {
		t.Fatalf("bad tool call event: %+v", events[0])
	}
	if events[1].Tool == nil || events[1].Tool.Name != "exec" || events[1].Tool.Text != "tool output" {
		t.Fatalf("bad tool result event: %+v", events[1])
	}
}

func TestRunResponseToolCallEventRedactsSecrets(t *testing.T) {
	srv := anthropicServer(t,
		`{"content":[{"type":"tool_use","id":"toolu_1","name":"sleep","input":{"duration":"1ms","note":"s3cr3t-token"}}],"stop_reason":"tool_use"}`,
		`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`,
	)
	client := agent.NewClient("k", agent.WithBaseURL(srv.URL))
	reg, err := tool.NewRegistry([]agentmd.ToolGrant{{Name: "sleep"}}, []string{"s3cr3t-token"})
	if err != nil {
		t.Fatal(err)
	}
	r := New(client, testDefinition(), "", output.Author{})
	r.SetTools(reg)

	var events []a2a.Event
	if _, err := r.Run(context.Background(), a2a.InvokeRequest{Prompt: "hi"}, func(ev a2a.Event) {
		events = append(events, ev)
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(events) == 0 || events[0].Type != a2a.EventToolCall || events[0].Tool == nil {
		t.Fatalf("want tool_call event first, got %+v", events)
	}
	if strings.Contains(events[0].Tool.Text, "s3cr3t-token") {
		t.Fatalf("secret leaked in tool_call event: %q", events[0].Tool.Text)
	}
	if !strings.Contains(events[0].Tool.Text, "***") {
		t.Fatalf("want redacted secret in tool_call event, got %q", events[0].Tool.Text)
	}
}

func TestRunResponseWithoutToolsKeepsSingleCompletion(t *testing.T) {
	srv := anthropicServer(t,
		`{"content":[{"type":"text","text":"plain"}],"stop_reason":"end_turn"}`,
	)
	client := agent.NewClient("k", agent.WithBaseURL(srv.URL))
	r := New(client, testDefinition(), "", output.Author{})

	var events []a2a.Event
	result, err := r.Run(context.Background(), a2a.InvokeRequest{Prompt: "hi"}, func(ev a2a.Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "plain" {
		t.Fatalf("got %q", result.Output)
	}
	for _, ev := range events {
		if ev.Type == a2a.EventToolCall || ev.Type == a2a.EventToolResult {
			t.Fatalf("unexpected tool event without registry: %+v", ev)
		}
	}
	if len(events) != 1 || events[0].Type != a2a.EventArtifact {
		t.Fatalf("want single artifact event, got %+v", events)
	}
}
