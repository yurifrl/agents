// Package output implements the externalization contracts (pr, commit,
// response). The pr/commit wrappers use go-git for clone/commit/push and the
// GitHub REST API to open a pull request, so no git/gh binaries are required.
package output

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/yurifrl/agents/internal/agent"
)

// Author is the git identity used for commits.
type Author struct {
	Name  string
	Email string
}

// PR clones repo, applies the change on a new branch, pushes it, and (when
// openPR is true) opens a pull request. It returns the PR HTML URL, or the
// branch compare URL for commit-only runs.
func PR(ctx context.Context, repo, token, base string, change *agent.Change, author Author, openPR bool) (string, error) {
	owner, name, err := parseRepo(repo)
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "agent-pr-*")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	auth := &githttp.BasicAuth{Username: "git", Password: token}
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, name)

	repoG, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           cloneURL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(base),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return "", fmt.Errorf("clone %s: %w", cloneURL, err)
	}

	wt, err := repoG.Worktree()
	if err != nil {
		return "", err
	}

	branch := sanitizeBranch(change.Branch)
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: true,
	}); err != nil {
		return "", fmt.Errorf("checkout branch %q: %w", branch, err)
	}

	if err := applyFiles(dir, change.Files); err != nil {
		return "", err
	}
	if _, err := wt.Add("."); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	commitMsg := change.Title
	if commitMsg == "" {
		commitMsg = change.Summary
	}
	if _, err := wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: author.Name, Email: author.Email, When: time.Now()},
	}); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	if err := repoG.PushContext(ctx, &git.PushOptions{
		Auth:     auth,
		RefSpecs: []config.RefSpec{config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))},
	}); err != nil {
		return "", fmt.Errorf("push: %w", err)
	}

	if !openPR {
		return fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", owner, name, base, branch), nil
	}
	return openPRReq(ctx, owner, name, token, base, branch, change)
}

func applyFiles(dir string, files []agent.FileChange) error {
	for _, f := range files {
		clean := filepath.Clean(f.Path)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("refusing unsafe path %q", f.Path)
		}
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", f.Path, err)
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}

func openPRReq(ctx context.Context, owner, name, token, base, head string, change *agent.Change) (string, error) {
	title := change.Title
	if title == "" {
		title = change.Summary
	}
	payload, _ := json.Marshal(map[string]string{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  change.Body,
	})

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create pr: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create pr returned %d: %s", resp.StatusCode, string(body))
	}

	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", err
	}
	return pr.HTMLURL, nil
}

// parseRepo accepts "owner/repo" or a full https github URL.
func parseRepo(repo string) (owner, name string, err error) {
	r := strings.TrimSpace(repo)
	r = strings.TrimPrefix(r, "https://github.com/")
	r = strings.TrimPrefix(r, "git@github.com:")
	r = strings.TrimSuffix(r, ".git")
	parts := strings.Split(strings.Trim(r, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q (want owner/repo)", repo)
	}
	return parts[0], parts[1], nil
}

func sanitizeBranch(b string) string {
	b = strings.TrimSpace(b)
	if b == "" {
		return fmt.Sprintf("agent/change-%d", time.Now().Unix())
	}
	b = strings.ReplaceAll(b, " ", "-")
	return "agent/" + strings.TrimPrefix(b, "agent/")
}
