package agenttask

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/agentmd"
)

func newSkillClient(t *testing.T, objs ...agentsv1alpha1.AgentSkill) *clientfake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	b := clientfake.NewClientBuilder().WithScheme(scheme)
	for i := range objs {
		b = b.WithObjects(&objs[i])
	}
	return b
}

func skill(name string, labels map[string]string, instructions string, tools ...agentmd.ToolGrant) agentsv1alpha1.AgentSkill {
	return agentsv1alpha1.AgentSkill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec: agentsv1alpha1.AgentSkillSpec{
			Description:  "skill " + name,
			Instructions: instructions,
			Tools:        tools,
		},
	}
}

func skillNames(skills []agentsv1alpha1.AgentSkill) []string {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

func TestResolveSkillsExplicitOrderPreserved(t *testing.T) {
	c := newSkillClient(t,
		skill("alpha", nil, "A."),
		skill("bravo", nil, "B."),
	).Build()

	got, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{
		{Name: "bravo"},
		{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if want := []string{"bravo", "alpha"}; !reflect.DeepEqual(skillNames(got), want) {
		t.Fatalf("got %v, want %v", skillNames(got), want)
	}
}

func TestResolveSkillsSelectorSortedByName(t *testing.T) {
	c := newSkillClient(t,
		skill("charlie", map[string]string{"team": "sec"}, "C."),
		skill("alpha", map[string]string{"team": "sec"}, "A."),
		skill("bravo", map[string]string{"team": "sec"}, "B."),
		skill("other", map[string]string{"team": "ops"}, "O."),
	).Build()

	got, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{
		{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "sec"}}},
	})
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if want := []string{"alpha", "bravo", "charlie"}; !reflect.DeepEqual(skillNames(got), want) {
		t.Fatalf("got %v, want %v", skillNames(got), want)
	}
}

func TestResolveSkillsDuplicatesResolveOnce(t *testing.T) {
	c := newSkillClient(t,
		skill("one", map[string]string{"group": "x"}, "1."),
		skill("two", map[string]string{"group": "x"}, "2."),
	).Build()

	got, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{
		{Name: "two"},
		{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "x"}}},
		{Name: "one"},
	})
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if want := []string{"two", "one"}; !reflect.DeepEqual(skillNames(got), want) {
		t.Fatalf("got %v, want %v", skillNames(got), want)
	}
}

func TestResolveSkillsMissingExplicitFails(t *testing.T) {
	c := newSkillClient(t).Build()

	_, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{{Name: "ghost"}})
	if err == nil {
		t.Fatal("expected error for missing skill, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q should name the missing skill", err)
	}
}

func TestResolveSkillsEmptySelectorYieldsZero(t *testing.T) {
	c := newSkillClient(t, skill("one", map[string]string{"team": "sec"}, "1.")).Build()

	got, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{
		{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "nope"}}},
	})
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d skills, want 0", len(got))
	}
}

func TestResolveSkillsInvalidRef(t *testing.T) {
	c := newSkillClient(t, skill("one", nil, "1.")).Build()

	for name, ref := range map[string]agentsv1alpha1.SkillRef{
		"neither": {},
		"both":    {Name: "one", Selector: &metav1.LabelSelector{}},
	} {
		if _, err := ResolveSkills(context.Background(), c, "ns", []agentsv1alpha1.SkillRef{ref}); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}

func testDef() *agentmd.Definition {
	return &agentmd.Definition{
		Name:        "worker",
		Instruction: "You are a worker.",
		Tools: []agentmd.ToolGrant{
			{Name: "exec", Allow: [][]string{{"ls"}}, MaxCalls: 10, Timeout: "30s", Cooldown: "1s"},
		},
	}
}

func testSkills() []agentsv1alpha1.AgentSkill {
	return []agentsv1alpha1.AgentSkill{
		skill("one", nil, "Summarize findings.",
			agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"ls"}, {"pwd"}}, MaxCalls: 5, Timeout: "10s", Cooldown: "5s"},
			agentmd.ToolGrant{Name: "http", AllowHosts: []string{"example.com"}},
		),
		skill("two", nil, "File issues.",
			agentmd.ToolGrant{Name: "exec", Allow: [][]string{{"whoami"}}},
		),
	}
}

func TestComposeAppendsSkillSections(t *testing.T) {
	b := Compose(testDef(), testSkills())

	want := "You are a worker.\n\n## Skill: one\n\nSummarize findings.\n\n## Skill: two\n\nFile issues."
	if b.Instruction != want {
		t.Fatalf("instruction:\n%q\nwant:\n%q", b.Instruction, want)
	}
	if want := []string{"one", "two"}; !reflect.DeepEqual(b.SkillNames, want) {
		t.Fatalf("skill names: got %v, want %v", b.SkillNames, want)
	}
	if b.Definition == nil || b.Definition.Name != "worker" {
		t.Fatalf("bundle definition not carried through: %+v", b.Definition)
	}
	if b.Hash == "" {
		t.Fatal("hash must not be empty")
	}
}

func TestComposeMergesGrants(t *testing.T) {
	b := Compose(testDef(), testSkills())

	if len(b.Grants) != 2 {
		t.Fatalf("got %d grants, want 2: %+v", len(b.Grants), b.Grants)
	}
	exec := b.Grants[0]
	if exec.Name != "exec" {
		t.Fatalf("grants[0] = %q, want exec", exec.Name)
	}
	if want := [][]string{{"ls"}, {"pwd"}, {"whoami"}}; !reflect.DeepEqual(exec.Allow, want) {
		t.Fatalf("allow: got %v, want %v", exec.Allow, want)
	}
	if exec.MaxCalls != 5 {
		t.Fatalf("maxCalls: got %d, want 5 (strictest non-zero)", exec.MaxCalls)
	}
	if exec.Timeout != "10s" {
		t.Fatalf("timeout: got %q, want 10s (shortest non-zero)", exec.Timeout)
	}
	if exec.Cooldown != "5s" {
		t.Fatalf("cooldown: got %q, want 5s (longest non-zero)", exec.Cooldown)
	}
	if b.Grants[1].Name != "http" {
		t.Fatalf("grants[1] = %q, want http", b.Grants[1].Name)
	}
}

func TestComposeHashChanges(t *testing.T) {
	base := Compose(testDef(), testSkills()).Hash
	if again := Compose(testDef(), testSkills()).Hash; again != base {
		t.Fatal("hash must be deterministic for identical inputs")
	}

	skillEdited := testSkills()
	skillEdited[0].Spec.Instructions = "Changed."
	if h := Compose(testDef(), skillEdited).Hash; h == base {
		t.Fatal("hash must change when skill content changes")
	}

	defEdited := testDef()
	defEdited.Instruction = "Changed."
	if h := Compose(defEdited, testSkills()).Hash; h == base {
		t.Fatal("hash must change when definition content changes")
	}

	defToolEdited := testDef()
	defToolEdited.Tools[0].MaxCalls = 3
	if h := Compose(defToolEdited, testSkills()).Hash; h == base {
		t.Fatal("hash must change when definition tools change")
	}
}

func TestMergeGrants(t *testing.T) {
	tests := []struct {
		name string
		a, b []agentmd.ToolGrant
		want []agentmd.ToolGrant
	}{
		{
			name: "union preserves first-seen order",
			a:    []agentmd.ToolGrant{{Name: "exec"}, {Name: "http"}},
			b:    []agentmd.ToolGrant{{Name: "exec"}, {Name: "notify"}},
			want: []agentmd.ToolGrant{{Name: "exec"}, {Name: "http"}, {Name: "notify"}},
		},
		{
			name: "allow rules union and dedup in order",
			a:    []agentmd.ToolGrant{{Name: "exec", Allow: [][]string{{"ls"}, {"pwd"}}}},
			b:    []agentmd.ToolGrant{{Name: "exec", Allow: [][]string{{"pwd"}, {"whoami"}}}},
			want: []agentmd.ToolGrant{{Name: "exec", Allow: [][]string{{"ls"}, {"pwd"}, {"whoami"}}}},
		},
		{
			name: "allow hosts union and dedup in order",
			a:    []agentmd.ToolGrant{{Name: "http", AllowHosts: []string{"a.com", "b.com"}}},
			b:    []agentmd.ToolGrant{{Name: "http", AllowHosts: []string{"b.com", "c.com"}}},
			want: []agentmd.ToolGrant{{Name: "http", AllowHosts: []string{"a.com", "b.com", "c.com"}}},
		},
		{
			name: "strictest non-zero max calls",
			a:    []agentmd.ToolGrant{{Name: "exec", MaxCalls: 10}},
			b:    []agentmd.ToolGrant{{Name: "exec", MaxCalls: 5}},
			want: []agentmd.ToolGrant{{Name: "exec", MaxCalls: 5}},
		},
		{
			name: "zero max calls does not weaken a limit",
			a:    []agentmd.ToolGrant{{Name: "exec", MaxCalls: 5}},
			b:    []agentmd.ToolGrant{{Name: "exec"}},
			want: []agentmd.ToolGrant{{Name: "exec", MaxCalls: 5}},
		},
		{
			name: "shortest non-zero timeout",
			a:    []agentmd.ToolGrant{{Name: "http", Timeout: "30s"}},
			b:    []agentmd.ToolGrant{{Name: "http", Timeout: "10s"}},
			want: []agentmd.ToolGrant{{Name: "http", Timeout: "10s"}},
		},
		{
			name: "empty timeout does not shorten",
			a:    []agentmd.ToolGrant{{Name: "http", Timeout: "30s"}},
			b:    []agentmd.ToolGrant{{Name: "http"}},
			want: []agentmd.ToolGrant{{Name: "http", Timeout: "30s"}},
		},
		{
			name: "zero timeout does not replace a non-zero timeout",
			a:    []agentmd.ToolGrant{{Name: "http", Timeout: "30s"}},
			b:    []agentmd.ToolGrant{{Name: "http", Timeout: "0s"}},
			want: []agentmd.ToolGrant{{Name: "http", Timeout: "30s"}},
		},
		{
			name: "longest non-zero cooldown",
			a:    []agentmd.ToolGrant{{Name: "notify", Cooldown: "1s"}},
			b:    []agentmd.ToolGrant{{Name: "notify", Cooldown: "5s"}},
			want: []agentmd.ToolGrant{{Name: "notify", Cooldown: "5s"}},
		},
		{
			name: "url from env first non-empty wins",
			a:    []agentmd.ToolGrant{{Name: "notify"}},
			b:    []agentmd.ToolGrant{{Name: "notify", URLFromEnv: "HOOK_URL"}},
			want: []agentmd.ToolGrant{{Name: "notify", URLFromEnv: "HOOK_URL"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MergeGrants(tt.a, tt.b); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
