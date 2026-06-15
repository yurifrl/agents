package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FileChange is a full-file write the agent wants to make.
type FileChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Change is a structured edit plan produced by the agent.
type Change struct {
	Summary string       `json:"summary"`
	Branch  string       `json:"branch"`
	Title   string       `json:"title"`
	Body    string       `json:"body"`
	Files   []FileChange `json:"files"`
}

const planSystem = `You are a coding agent. Produce a concrete change as STRICT JSON only.
Schema: {"summary": string, "branch": string (kebab-case, no slashes), "title": string (PR title), "body": string (PR description, markdown), "files": [{"path": string, "content": string}]}.
Rules: full file contents (not diffs); only include files you are creating or replacing; no prose outside the JSON; do not wrap the JSON in code fences.`

// Plan asks the model to turn a task into a structured Change.
func (c *Client) Plan(ctx context.Context, model, prompt, repoContext string) (*Change, error) {
	full := prompt
	if repoContext != "" {
		full = prompt + "\n\nRepository context:\n" + repoContext
	}

	raw, err := c.Run(ctx, Request{Model: model, Prompt: full, System: planSystem})
	if err != nil {
		return nil, err
	}

	jsonText := extractJSON(raw)
	var change Change
	if err := json.Unmarshal([]byte(jsonText), &change); err != nil {
		return nil, fmt.Errorf("model did not return valid change JSON: %w\n--- raw ---\n%s", err, raw)
	}
	if len(change.Files) == 0 {
		return nil, fmt.Errorf("model returned no file changes")
	}
	return &change, nil
}

// extractJSON strips optional ```json fences and leading/trailing prose.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
