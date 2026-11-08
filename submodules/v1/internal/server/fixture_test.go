package server

import (
	"fmt"
	"os"
	"path/filepath"
)

// ompFixtureMode selects a behavior profile for the scripted worker.
type ompFixtureMode string

const (
	fixtureSettled ompFixtureMode = "settled" // ready → prompt → activity → agent_settled
	fixtureQuiet   ompFixtureMode = "quiet"   // agent_end only; get_state reports idle (settle-quiet fallback)
	fixtureCrash   ompFixtureMode = "crash"   // ready then exit 7
	fixtureHang    ompFixtureMode = "hang"    // ready then sleep forever (deadline test)
	fixtureSteer   ompFixtureMode = "steer"   // steer → agent_start + agent_end, then settles quietly
	fixtureBusy    ompFixtureMode = "busy"    // stays running (isStreaming true); consumes stdin promptly
)

// writeOmpFixture emits an executable POSIX-sh fake omp implementing the
// line-JSON RPC surface (CONTRACT.md §2) for the given mode.
func writeOmpFixture(t testingT, dir string, mode ompFixtureMode) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
# fake-omp (%s): scripted worker for server tests
FAKE_STREAMING=%s
echo '{"type":"ready","protocolVersion":1,"sessionFile":"%s"}'
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  type=$(printf '%%s' "$line" | sed -n 's/.*"type":"\([a-z_]*\)".*/\1/p')
  case "$type" in
    prompt)
      printf '{"id":%%s,"type":"response","success":true}\n' "$id"
%s
      ;;
    get_state)
      if [ "$FAKE_STREAMING" = "1" ]; then
        printf '{"id":%%s,"type":"response","success":true,"isStreaming":true}\n' "$id"
      else
        printf '{"id":%%s,"type":"response","success":true,"isStreaming":false}\n' "$id"
      fi
      ;;
    get_last_assistant_text)
      printf '{"id":%%s,"type":"response","success":true,"text":"final answer text"}\n' "$id"
      ;;
    steer)
      printf '{"id":%%s,"type":"response","success":true}\n' "$id"
      ;;
    abort)
      printf '{"id":%%s,"type":"response","success":true}\n' "$id"
      ;;
    *)
      printf '{"id":%%s,"type":"response","success":true}\n' "$id"
      ;;
  esac
done
`, mode, fixtureStreaming(mode), filepath.Join(dir, "session.jsonl"), fixtureBody(mode))
	path := filepath.Join(dir, fmt.Sprintf("fake-omp-%s.sh", mode))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// fixtureStreaming reports whether the mode keeps the agent "streaming"
// on get_state (busy stays running; everything else settles when idle).
func fixtureStreaming(mode ompFixtureMode) string {
	if mode == fixtureBusy || mode == fixtureHang {
		return "1"
	}
	return "0"
}

func fixtureBody(mode ompFixtureMode) string {
	switch mode {
	case fixtureSettled:
		return `echo '{"type":"tool_execution_start","toolName":"read_file","args":{"path":"x"}}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}'
echo '{"type":"agent_settled"}'`
	case fixtureQuiet:
		return `echo '{"type":"agent_end"}'`
	case fixtureCrash:
		return `echo 'crashing' ; exit 7`
	case fixtureHang:
		return `sleep 300`
	case fixtureSteer:
		return `echo '{"type":"agent_start"}'
echo '{"type":"agent_end"}'`
	case fixtureBusy:
		return `: # stay running; get_state reports streaming`
	}
	return ""
}

// testingT is the minimal testing surface the helpers need (keeps this file
// usable from package tests without importing testing in production code).
type testingT interface {
	Fatalf(format string, args ...any)
	Helper()
}
