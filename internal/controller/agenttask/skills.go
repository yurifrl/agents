package agenttask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/agentmd"
)

//+kubebuilder:rbac:groups=agents.yurifrl.dev,resources=agentskills,verbs=get;list;watch

// ResolvedBundle is the deterministic result of composing an agent definition
// with its resolved skills: the merged instruction, merged tool grants, and a
// content hash that changes whenever any input changes.
type ResolvedBundle struct {
	Definition  *agentmd.Definition
	Instruction string
	Grants      []agentmd.ToolGrant
	SkillNames  []string
	Hash        string
}

// ResolveSkills resolves skill refs against the API server in declaration
// order. Explicit refs preserve their order, selector results are sorted by
// namespace/name, and a skill selected more than once resolves once. A missing
// explicit skill is an error; an empty selector result is valid.
func ResolveSkills(ctx context.Context, c client.Client, ns string, refs []agentsv1alpha1.SkillRef) ([]agentsv1alpha1.AgentSkill, error) {
	var out []agentsv1alpha1.AgentSkill
	seen := map[string]bool{}
	add := func(s agentsv1alpha1.AgentSkill) {
		key := s.Namespace + "/" + s.Name
		if !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}

	for i, ref := range refs {
		switch {
		case (ref.Name == "") == (ref.Selector == nil):
			return nil, fmt.Errorf("skills[%d]: exactly one of name or selector must be set", i)
		case ref.Name != "":
			var s agentsv1alpha1.AgentSkill
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s); err != nil {
				return nil, fmt.Errorf("resolving skill %q: %w", ref.Name, err)
			}
			add(s)
		default:
			sel, err := metav1.LabelSelectorAsSelector(ref.Selector)
			if err != nil {
				return nil, fmt.Errorf("skills[%d]: invalid selector: %w", i, err)
			}
			var list agentsv1alpha1.AgentSkillList
			if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
				return nil, fmt.Errorf("listing skills: %w", err)
			}
			sort.Slice(list.Items, func(a, b int) bool {
				return list.Items[a].Namespace+"/"+list.Items[a].Name < list.Items[b].Namespace+"/"+list.Items[b].Name
			})
			for _, s := range list.Items {
				add(s)
			}
		}
	}
	return out, nil
}

// Compose merges a definition with its resolved skills into a deterministic
// bundle: skill instructions are appended as "## Skill: <name>" sections and
// tool grants are merged in declaration order.
func Compose(def *agentmd.Definition, skills []agentsv1alpha1.AgentSkill) *ResolvedBundle {
	b := &ResolvedBundle{Definition: def, Instruction: def.Instruction}

	grants := def.Tools
	for _, s := range skills {
		b.Instruction += "\n\n## Skill: " + s.Name + "\n\n" + strings.TrimSpace(s.Spec.Instructions)
		b.SkillNames = append(b.SkillNames, s.Name)
		grants = MergeGrants(grants, s.Spec.Tools)
	}
	b.Grants = grants
	b.Hash = bundleHash(def, skills, b.Instruction)
	return b
}

// bundleHash hashes everything that identifies the resolved content, so any
// change to the definition or a resolved skill changes the hash.
func bundleHash(def *agentmd.Definition, skills []agentsv1alpha1.AgentSkill, instruction string) string {
	type skillContent struct {
		Name string                        `json:"name"`
		Spec agentsv1alpha1.AgentSkillSpec `json:"spec"`
	}
	payload := struct {
		Definition  *agentmd.Definition `json:"definition"`
		Instruction string              `json:"instruction"`
		Skills      []skillContent      `json:"skills"`
	}{Definition: def, Instruction: instruction}
	for _, s := range skills {
		payload.Skills = append(payload.Skills, skillContent{Name: s.Name, Spec: s.Spec})
	}
	// agentmd.Definition and AgentSkillSpec marshal deterministically (fixed
	// field order, no maps).
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// MergeGrants unions two grant lists by tool name. Grants for the same tool
// merge: allow rules and hosts union (dedup, first-seen order), max calls
// takes the strictest non-zero value, timeout the shortest non-zero, cooldown
// the longest non-zero.
func MergeGrants(a, b []agentmd.ToolGrant) []agentmd.ToolGrant {
	out := append([]agentmd.ToolGrant(nil), a...)
	index := make(map[string]int, len(out))
	for i, g := range out {
		index[g.Name] = i
	}
	for _, g := range b {
		i, ok := index[g.Name]
		if !ok {
			index[g.Name] = len(out)
			out = append(out, g)
			continue
		}
		out[i] = mergeGrant(out[i], g)
	}
	return out
}

func mergeGrant(a, b agentmd.ToolGrant) agentmd.ToolGrant {
	a.Allow = unionRules(a.Allow, b.Allow)
	a.AllowHosts = unionStrings(a.AllowHosts, b.AllowHosts)
	if a.URLFromEnv == "" {
		a.URLFromEnv = b.URLFromEnv
	}
	if b.MaxCalls > 0 && (a.MaxCalls == 0 || b.MaxCalls < a.MaxCalls) {
		a.MaxCalls = b.MaxCalls
	}
	a.Timeout = pickDuration(a.Timeout, b.Timeout, func(x, y time.Duration) bool { return x < y })
	a.Cooldown = pickDuration(a.Cooldown, b.Cooldown, func(x, y time.Duration) bool { return x > y })
	return a
}

func unionRules(a, b [][]string) [][]string {
	out := append([][]string(nil), a...)
	seen := map[string]bool{}
	for _, r := range a {
		seen[strings.Join(r, "\x00")] = true
	}
	for _, r := range b {
		key := strings.Join(r, "\x00")
		if !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// pickDuration returns the better of two duration strings per keep (min for
// timeouts, max for cooldowns); empty and zero mean unset and never win.
func pickDuration(a, b string, keep func(x, y time.Duration) bool) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	da, errA := time.ParseDuration(a)
	db, errB := time.ParseDuration(b)
	if errA != nil || errB != nil {
		return a
	}
	if da == 0 {
		return b
	}
	if db == 0 {
		return a
	}
	if keep(db, da) {
		return b
	}
	return a
}
