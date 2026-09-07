package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/agentmd"
	"github.com/yurifrl/agents/internal/tool"
)

// fakeAnthropic is a scripted Anthropic Messages API server. Each request pops
// the next scripted response; requests are captured for assertions.
type fakeAnthropic struct {
	mu       sync.Mutex
	requests []messagesRequest
	raw      [][]byte
	script   []scriptedResponse
	block    chan struct{}
}

type scriptedResponse struct {
	status int
	body   string
}

func newFakeAnthropic(t *testing.T, script ...scriptedResponse) (*fakeAnthropic, *httptest.Server) {
	f := &fakeAnthropic{script: script}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req messagesRequest
		var raw json.RawMessage
		body := json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("reading request body: %v", err)
		}
		raw = body
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.raw = append(f.raw, raw)
		f.mu.Unlock()

		if f.block != nil {
			select {
			case <-f.block:
			case <-r.Context().Done():
				return
			}
		}

		f.mu.Lock()
		if len(f.script) == 0 {
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			t.Errorf("unexpected extra request")
			return
		}
		next := f.script[0]
		f.script = f.script[1:]
		f.mu.Unlock()

		status := next.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(next.body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAnthropic) request(i int) messagesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeAnthropic) numRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func textResp(text string) scriptedResponse {
	return scriptedResponse{body: `{"content":[{"type":"text","text":` + jsonString(text) + `}],"stop_reason":"end_turn"}`}
}

func toolUseResp(id, name, input string) scriptedResponse {
	return scriptedResponse{body: `{"content":[{"type":"tool_use","id":` + jsonString(id) + `,"name":` + jsonString(name) + `,"input":` + input + `}],"stop_reason":"tool_use"}`}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type fakeTools struct {
	schemas []json.RawMessage
	results map[string]tool.Result
	calls   []tool.Call
}

func (f *fakeTools) Schemas() []json.RawMessage { return f.schemas }

func (f *fakeTools) Execute(_ context.Context, c tool.Call) tool.Result {
	f.calls = append(f.calls, c)
	if r, ok := f.results[c.Name]; ok {
		return r
	}
	return tool.Result{Text: "ok"}
}

func TestRunSingleCompletion(t *testing.T) {
	f, srv := newFakeAnthropic(t, textResp("hello world"))
	client := NewClient("test-key", WithBaseURL(srv.URL))

	out, err := client.Run(context.Background(), Request{Model: "claude-test", Prompt: "say hi", System: "be nice"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("got %q, want %q", out, "hello world")
	}

	req := f.request(0)
	if req.Model != "claude-test" || req.System != "be nice" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", req.Messages)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "say hi" {
		t.Fatalf("user message must be a text content block, got %+v", blocks)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("no tools expected, got %d", len(req.Tools))
	}
}

func TestRunLoopToolRoundTrip(t *testing.T) {
	f, srv := newFakeAnthropic(t,
		toolUseResp("toolu_1", "exec", `{"argv":["echo","hi"]}`),
		textResp("the command said hi"),
	)
	client := NewClient("test-key", WithBaseURL(srv.URL))
	tools := &fakeTools{
		schemas: []json.RawMessage{json.RawMessage(`{"name":"exec","description":"run"}`)},
		results: map[string]tool.Result{"exec": {Text: "hi\n"}},
	}

	out, err := client.RunLoop(context.Background(), LoopRequest{Model: "claude-test", Prompt: "run echo", Tools: tools})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if out != "the command said hi" {
		t.Fatalf("got %q", out)
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "exec" {
		t.Fatalf("unexpected tool calls: %+v", tools.calls)
	}
	if string(tools.calls[0].Arguments) != `{"argv":["echo","hi"]}` {
		t.Fatalf("unexpected arguments: %s", tools.calls[0].Arguments)
	}

	first := f.request(0)
	if len(first.Tools) != 1 {
		t.Fatalf("tools schemas not sent: %+v", first.Tools)
	}

	second := f.request(1)
	if len(second.Messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant, tool results), got %d", len(second.Messages))
	}
	assistant := second.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" {
		t.Fatalf("assistant tool_use turn missing: %+v", assistant)
	}
	results := second.Messages[2]
	if results.Role != "user" || len(results.Content) != 1 {
		t.Fatalf("tool result turn missing: %+v", results)
	}
	tr := results.Content[0]
	if tr.Type != "tool_result" || tr.ToolUseID != "toolu_1" || tr.Content != "hi\n" || tr.IsError {
		t.Fatalf("bad tool_result block: %+v", tr)
	}
}

func TestRunLoopMultiTurn(t *testing.T) {
	f, srv := newFakeAnthropic(t,
		toolUseResp("toolu_1", "exec", `{"argv":["ls"]}`),
		toolUseResp("toolu_2", "exec", `{"argv":["pwd"]}`),
		textResp("all done"),
	)
	client := NewClient("test-key", WithBaseURL(srv.URL))
	tools := &fakeTools{}

	out, err := client.RunLoop(context.Background(), LoopRequest{Model: "m", Prompt: "p", Tools: tools})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if out != "all done" {
		t.Fatalf("got %q", out)
	}
	if len(tools.calls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(tools.calls))
	}
	if f.numRequests() != 3 {
		t.Fatalf("want 3 API requests, got %d", f.numRequests())
	}
	// Third request must carry both tool_result turns.
	msgs := f.request(2).Messages
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(msgs), msgs)
	}
}

func TestRunLoopRegistryErrorsSurfaced(t *testing.T) {
	// Unknown tool then invalid arguments: both must reach the model as
	// is_error tool_result blocks so it can recover.
	registry, err := tool.NewRegistry([]agentmd.ToolGrant{{Name: "exec", Allow: [][]string{{"echo"}}}}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	f, srv := newFakeAnthropic(t,
		toolUseResp("toolu_1", "rm-rf", `{}`),
		toolUseResp("toolu_2", "exec", `{"argv":123}`),
		textResp("recovered"),
	)
	client := NewClient("test-key", WithBaseURL(srv.URL))

	out, err := client.RunLoop(context.Background(), LoopRequest{Model: "m", Prompt: "p", Tools: registry})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if out != "recovered" {
		t.Fatalf("got %q", out)
	}

	final := f.request(2).Messages
	var errs []contentBlock
	for _, m := range final {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.IsError {
				errs = append(errs, b)
			}
		}
	}
	if len(errs) != 2 {
		t.Fatalf("want 2 error tool_results, got %d: %+v", len(errs), final)
	}
	if !strings.Contains(errs[0].Content, "not granted") {
		t.Fatalf("unknown tool error not surfaced: %q", errs[0].Content)
	}
	if !strings.Contains(errs[1].Content, "invalid arguments") {
		t.Fatalf("invalid args error not surfaced: %q", errs[1].Content)
	}
}

func TestRunLoopMaxTurns(t *testing.T) {
	script := make([]scriptedResponse, 3)
	for i := range script {
		script[i] = toolUseResp("toolu_x", "exec", `{}`)
	}
	f, srv := newFakeAnthropic(t, script...)
	client := NewClient("test-key", WithBaseURL(srv.URL))

	_, err := client.RunLoop(context.Background(), LoopRequest{
		Model: "m", Prompt: "p", MaxTurns: 3, Tools: &fakeTools{},
	})
	if err == nil || !strings.Contains(err.Error(), "max turns") {
		t.Fatalf("want max turns error, got %v", err)
	}
	if f.numRequests() != 3 {
		t.Fatalf("want 3 requests, got %d", f.numRequests())
	}
}

func TestRunLoopCancellation(t *testing.T) {
	f, srv := newFakeAnthropic(t, textResp("never"))
	f.block = make(chan struct{})
	client := NewClient("test-key", WithBaseURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.RunLoop(ctx, LoopRequest{Model: "m", Prompt: "p", Tools: &fakeTools{}})
	if err == nil {
		t.Fatal("want cancellation error, got nil")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation took too long: %s", time.Since(start))
	}
}

func TestRunLoopEventCallback(t *testing.T) {
	_, srv := newFakeAnthropic(t,
		toolUseResp("toolu_1", "exec", `{"argv":["ls"]}`),
		textResp("done"),
	)
	client := NewClient("test-key", WithBaseURL(srv.URL))

	type event struct {
		name     string
		isResult bool
		text     string
	}
	var events []event
	_, err := client.RunLoop(context.Background(), LoopRequest{
		Model: "m", Prompt: "p",
		Tools: &fakeTools{results: map[string]tool.Result{"exec": {Text: "file.go"}}},
		OnToolEvent: func(name string, isResult bool, text string) {
			events = append(events, event{name, isResult, text})
		},
	})
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %+v", events)
	}
	if events[0].name != "exec" || events[0].isResult || !strings.Contains(events[0].text, "ls") {
		t.Fatalf("bad call event: %+v", events[0])
	}
	if events[1].name != "exec" || !events[1].isResult || events[1].text != "file.go" {
		t.Fatalf("bad result event: %+v", events[1])
	}
}

func TestRunLoopToolUseWithoutTools(t *testing.T) {
	_, srv := newFakeAnthropic(t, toolUseResp("toolu_1", "exec", `{}`))
	client := NewClient("test-key", WithBaseURL(srv.URL))

	_, err := client.RunLoop(context.Background(), LoopRequest{Model: "m", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "no tools") {
		t.Fatalf("want no-tools error, got %v", err)
	}
}

func TestRunLoopAPIError(t *testing.T) {
	_, srv := newFakeAnthropic(t, scriptedResponse{
		status: http.StatusBadRequest,
		body:   `{"error":{"type":"invalid_request_error","message":"bad request yo"}}`,
	})
	client := NewClient("test-key", WithBaseURL(srv.URL))

	_, err := client.RunLoop(context.Background(), LoopRequest{Model: "m", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "bad request yo") {
		t.Fatalf("want API error surfaced, got %v", err)
	}
}
