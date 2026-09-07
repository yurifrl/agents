package agentmd

import (
	"testing"
	"time"
)

func TestParseToolGrants(t *testing.T) {
	doc := `---
name: worker
tools:
  - name: read
  - name: http
    allow: [["GET", "POST"]]
    allowHosts: ["api.github.com"]
    urlFromEnv: API_BASE
    maxCalls: 10
    timeout: 30s
    cooldown: 1m
---

Do work.
`
	def, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(def.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(def.Tools))
	}
	read := def.Tools[0]
	if read.Name != "read" {
		t.Errorf("tool[0].Name = %q, want read", read.Name)
	}
	http := def.Tools[1]
	if http.Name != "http" {
		t.Errorf("tool[1].Name = %q, want http", http.Name)
	}
	if len(http.Allow) != 1 || len(http.Allow[0]) != 2 || http.Allow[0][0] != "GET" {
		t.Errorf("Allow = %v", http.Allow)
	}
	if len(http.AllowHosts) != 1 || http.AllowHosts[0] != "api.github.com" {
		t.Errorf("AllowHosts = %v", http.AllowHosts)
	}
	if http.URLFromEnv != "API_BASE" {
		t.Errorf("URLFromEnv = %q", http.URLFromEnv)
	}
	if http.MaxCalls != 10 {
		t.Errorf("MaxCalls = %d", http.MaxCalls)
	}
	if http.Timeout != "30s" {
		t.Errorf("Timeout = %q", http.Timeout)
	}
	if http.Cooldown != "1m" {
		t.Errorf("Cooldown = %q", http.Cooldown)
	}
}

func TestParseLoopInterval(t *testing.T) {
	doc := `---
name: obsidian
loop:
  interval: 5m
---

Watch the vault.
`
	def, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d, err := def.LoopInterval()
	if err != nil {
		t.Fatalf("LoopInterval: %v", err)
	}
	if d != 5*time.Minute {
		t.Errorf("LoopInterval = %v, want 5m", d)
	}
}

func TestParseLoopIntervalDefault(t *testing.T) {
	doc := `---
name: plain
---

Do a thing.
`
	def, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d, err := def.LoopInterval()
	if err != nil {
		t.Fatalf("LoopInterval: %v", err)
	}
	if d != 0 {
		t.Errorf("LoopInterval = %v, want 0 for no loop", d)
	}
}

func TestParseLoopIntervalInvalidDuration(t *testing.T) {
	doc := `---
name: bad
loop:
  interval: soon
---

Nope.
`
	if _, err := Parse(doc); err == nil {
		t.Fatal("Parse: want error for invalid duration")
	}
}

func TestParseToolGrantMissingName(t *testing.T) {
	doc := `---
name: bad
tools:
  - maxCalls: 3
---

Nope.
`
	if _, err := Parse(doc); err == nil {
		t.Fatal("Parse: want error for tool grant without name")
	}
}

func TestParseLoopEmptyInterval(t *testing.T) {
	doc := `---
name: bad
loop:
  interval: ""
---

Nope.
`
	if _, err := Parse(doc); err == nil {
		t.Fatal("Parse: want error for empty interval")
	}
}
