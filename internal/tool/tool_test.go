package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/agentmd"
)

func TestNewRegistryUnknownTool(t *testing.T) {
	_, err := NewRegistry([]agentmd.ToolGrant{{Name: "k8s-delete-pods"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("want unknown tool error, got %v", err)
	}
}

func TestNewRegistryBadDuration(t *testing.T) {
	_, err := NewRegistry([]agentmd.ToolGrant{{Name: "exec", Timeout: "not-a-duration"}}, nil)
	if err == nil {
		t.Fatal("want schema failure for bad timeout, got nil")
	}
}

func registry(t *testing.T, grant agentmd.ToolGrant, secrets []string) *Registry {
	t.Helper()
	r, err := NewRegistry([]agentmd.ToolGrant{grant}, secrets)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func TestSchemas(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}}, nil)
	if len(r.Schemas()) != 1 {
		t.Fatalf("want 1 schema, got %d", len(r.Schemas()))
	}
}

func TestExecuteUnknownCall(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}}, nil)
	res := r.Execute(context.Background(), Call{Name: "http"})
	if !res.IsError || !strings.Contains(res.Text, "not granted") {
		t.Fatalf("want not-granted error, got %+v", res)
	}
}

func TestExecArgvPrefixDenial(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}}, nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"rm", "-rf", "/"}})
	res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args})
	if !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Fatalf("want denial, got %+v", res)
	}
}

func TestExecArgvPrefixAllow(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}}, nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"echo", "hello"}})
	res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args})
	if res.IsError || !strings.Contains(res.Text, "hello") {
		t.Fatalf("want hello output, got %+v", res)
	}
}

func TestHTTPHostDenial(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "http", AllowHosts: []string{"example.com"}}, nil)
	args, _ := json.Marshal(map[string]any{"url": "http://evil.com/"})
	res := r.Execute(context.Background(), Call{Name: "http", Arguments: args})
	if !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Fatalf("want host denial, got %+v", res)
	}
}

func TestMaxCalls(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}, MaxCalls: 1}, nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"echo", "x"}})
	if res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args}); res.IsError {
		t.Fatalf("first call should succeed: %+v", res)
	}
	res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args})
	if !res.IsError || !strings.Contains(res.Text, "max calls") {
		t.Fatalf("want max calls error, got %+v", res)
	}
}

func TestTimeout(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "sleep", Timeout: "50ms"}, nil)
	args, _ := json.Marshal(map[string]any{"duration": "5s"})
	res := r.Execute(context.Background(), Call{Name: "sleep", Arguments: args})
	if !res.IsError {
		t.Fatalf("want timeout error, got %+v", res)
	}
}

func TestCooldown(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}, Cooldown: "1h"}, nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"echo", "x"}})
	if res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args}); res.IsError {
		t.Fatalf("first call should succeed: %+v", res)
	}
	res := r.Execute(context.Background(), Call{Name: "exec", Arguments: args})
	if !res.IsError || !strings.Contains(res.Text, "cooldown") {
		t.Fatalf("want cooldown error, got %+v", res)
	}
}

func TestSecretRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("token=supersecretvalue"))
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	r := registry(t, agentmd.ToolGrant{Name: "http", AllowHosts: []string{host}}, []string{"supersecretvalue"})
	args, _ := json.Marshal(map[string]any{"url": srv.URL})
	res := r.Execute(context.Background(), Call{Name: "http", Arguments: args})
	if res.IsError {
		t.Fatalf("http call failed: %+v", res)
	}
	if strings.Contains(res.Text, "supersecretvalue") {
		t.Fatalf("secret leaked in output: %q", res.Text)
	}
	if !strings.Contains(res.Text, "***") {
		t.Fatalf("want redaction marker, got %q", res.Text)
	}
}

func TestDeniedCallDoesNotConsumeQuota(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"echo"}}, MaxCalls: 1}, nil)
	denied, _ := json.Marshal(map[string]any{"argv": []string{"rm", "-rf", "/"}})
	if res := r.Execute(context.Background(), Call{Name: "exec", Arguments: denied}); !res.IsError {
		t.Fatalf("want denial, got %+v", res)
	}
	allowed, _ := json.Marshal(map[string]any{"argv": []string{"echo", "x"}})
	res := r.Execute(context.Background(), Call{Name: "exec", Arguments: allowed})
	if res.IsError {
		t.Fatalf("allowed call after denied call should succeed with MaxCalls 1, got %+v", res)
	}
}

func TestNotifyErrorDoesNotLeakURL(t *testing.T) {
	webhook := "http://127.0.0.1:1/secret-webhook-path"
	t.Setenv("LEAK_TEST_WEBHOOK", webhook)
	r := registry(t, agentmd.ToolGrant{Name: "notify", URLFromEnv: "LEAK_TEST_WEBHOOK"}, nil)
	args, _ := json.Marshal(map[string]any{"text": "hi"})
	res := r.Execute(context.Background(), Call{Name: "notify", Arguments: args})
	if !res.IsError {
		t.Fatalf("want notify failure, got %+v", res)
	}
	if strings.Contains(res.Text, webhook) {
		t.Fatalf("webhook URL leaked in error result: %q", res.Text)
	}
}

func TestSleepCancellable(t *testing.T) {
	r := registry(t, agentmd.ToolGrant{Name: "sleep"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	args, _ := json.Marshal(map[string]any{"duration": "10s"})
	res := r.Execute(ctx, Call{Name: "sleep", Arguments: args})
	if !res.IsError {
		t.Fatalf("want cancellation error, got %+v", res)
	}
}
