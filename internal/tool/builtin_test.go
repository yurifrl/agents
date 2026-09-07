package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yurifrl/agents/internal/agentmd"
)

func TestExecArgvOnly(t *testing.T) {
	e := NewExec()
	args, _ := json.Marshal(map[string]any{"argv": []string{"echo", "hello; rm -rf /"}})
	res, err := e.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "hello; rm -rf /") {
		t.Fatalf("argv must not be shell-interpreted, got %q", res.Text)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	e := NewExec()
	args, _ := json.Marshal(map[string]any{"argv": []string{"false"}})
	res, err := e.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("want IsError for non-zero exit, got %+v", res)
	}
}

func TestHTTPBoundedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 2<<20))
	}))
	t.Cleanup(srv.Close)
	h := NewHTTP([]string{strings.TrimPrefix(srv.URL, "http://")})
	args, _ := json.Marshal(map[string]any{"url": srv.URL})
	res, err := h.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > maxHTTPBody+64 {
		t.Fatalf("response not bounded: %d bytes", len(res.Text))
	}
}

func TestHTTPMethod(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		io.Copy(io.Discard, r.Body)
		fmt.Fprintln(w, "ok")
	}))
	t.Cleanup(srv.Close)
	h := NewHTTP([]string{strings.TrimPrefix(srv.URL, "http://")})
	args, _ := json.Marshal(map[string]any{"url": srv.URL, "method": "POST", "body": "hi"})
	if _, err := h.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Fatalf("want POST, got %q", gotMethod)
	}
}

func TestHTTPRedirectToDeniedHost(t *testing.T) {
	var deniedHit bool
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deniedHit = true
	}))
	t.Cleanup(denied.Close)
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, denied.URL, http.StatusFound)
	}))
	t.Cleanup(allowed.Close)

	allowedHost := strings.TrimPrefix(allowed.URL, "http://")
	h := NewHTTP([]string{allowedHost})
	args, _ := json.Marshal(map[string]any{"url": allowed.URL})
	_, err := h.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("want error for redirect to denied host")
	}
	if deniedHit {
		t.Fatal("redirect target on denied host was fetched")
	}
}

func TestNotifyWebhook(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TEST_WEBHOOK_URL", srv.URL)

	n, err := NewNotify("TEST_WEBHOOK_URL")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"text": "deploy done"})
	res, err := n.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("notify failed: %v %+v", err, res)
	}
	if !strings.Contains(string(got), "deploy done") {
		t.Fatalf("webhook did not receive text, got %q", got)
	}
	if strings.Contains(res.Text, srv.URL) {
		t.Fatalf("webhook URL leaked in result: %q", res.Text)
	}
}

func TestNotifyMissingEnv(t *testing.T) {
	_, err := NewNotify("DEFINITELY_UNSET_ENV_VAR")
	if err == nil {
		t.Fatal("want error when env var unset")
	}
}

func TestBuiltinSchemas(t *testing.T) {
	t.Setenv("SCHEMA_TEST_WEBHOOK", "http://localhost/hook")
	grants := map[string]agentmd.ToolGrant{
		"exec":   {Name: "exec"},
		"http":   {Name: "http"},
		"notify": {Name: "notify", URLFromEnv: "SCHEMA_TEST_WEBHOOK"},
		"sleep":  {Name: "sleep"},
	}
	for _, name := range []string{"exec", "http", "notify", "sleep"} {
		f, ok := builtins[name]
		if !ok {
			t.Fatalf("missing builtin %q", name)
		}
		tool, err := f(grants[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tool.Name() != name {
			t.Fatalf("want name %q, got %q", name, tool.Name())
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Fatalf("%s: invalid schema JSON: %v", name, err)
		}
		if _, ok := schema["input_schema"]; !ok {
			t.Fatalf("%s: schema must use Anthropic input_schema key, got %s", name, tool.Schema())
		}
		if _, ok := schema["parameters"]; ok {
			t.Fatalf("%s: schema uses OpenAI parameters key, Anthropic requires input_schema", name)
		}
	}
}
