package workstrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Bead is the subset of a beads issue the workstrator needs.
type Bead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// bd wraps the beads CLI. The CLI is assumed pre-configured (or configured via
// Init) to talk to the external Dolt server.
type bd struct {
	bin string
}

func newBD() *bd { return &bd{bin: "bd"} }

func (b *bd) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ClaimReady atomically claims all ready beads and returns them. Atomic claim is
// the dedup mechanism: a bead claimed here will not be returned to another
// poller.
func (b *bd) ClaimReady(ctx context.Context) ([]Bead, error) {
	out, err := b.run(ctx, "ready", "--claim", "--json")
	if err != nil {
		return nil, err
	}
	return parseBeads(out)
}

// Comment adds a comment to a bead.
func (b *bd) Comment(ctx context.Context, id, text string) error {
	_, err := b.run(ctx, "comment", id, text)
	return err
}

// Close closes a bead with a reason.
func (b *bd) Close(ctx context.Context, id, reason string) error {
	_, err := b.run(ctx, "close", id, "--reason="+reason)
	return err
}

// Unclaim releases a bead back to open (on failure).
func (b *bd) Unclaim(ctx context.Context, id string) error {
	_, err := b.run(ctx, "update", id, "--status", "open", "--assignee", "")
	return err
}

// parseBeads tolerates either a bare array or an object with an "issues"/"beads"
// field, since bd JSON shapes vary by subcommand/version.
func parseBeads(out []byte) ([]Bead, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var beads []Bead
		if err := json.Unmarshal([]byte(trimmed), &beads); err != nil {
			return nil, fmt.Errorf("parsing bd json array: %w", err)
		}
		return beads, nil
	}
	var wrapper struct {
		Issues []Bead `json:"issues"`
		Beads  []Bead `json:"beads"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
		return nil, fmt.Errorf("parsing bd json object: %w", err)
	}
	if len(wrapper.Issues) > 0 {
		return wrapper.Issues, nil
	}
	return wrapper.Beads, nil
}
