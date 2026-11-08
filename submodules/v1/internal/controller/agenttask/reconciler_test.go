package agenttask

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/agentmd"
	"github.com/yurifrl/agents/internal/config"
)

const inlineDefinition = `---
name: worker
tools:
  - name: exec
    allow: [["ls"]]
---
You are a worker.
`

func testConfig() *config.Config {
	return &config.Config{
		Namespace:              "agents",
		AgentImage:             "ghcr.io/yurifrl/agents:cfg",
		SecretName:             "agents",
		RestartBudget:          3,
		DefaultDeadlineSeconds: 3600,
	}
}

func newFakeReconciler(t *testing.T, objs ...runtime.Object) (*Reconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme agents: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	b := clientfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.AgentTask{})
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	c := b.Build()
	return NewReconciler(c, testConfig()), c
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestValidateMode(t *testing.T) {
	task := func(mut func(*agentsv1alpha1.AgentTaskSpec)) *agentsv1alpha1.AgentTask {
		spec := agentsv1alpha1.AgentTaskSpec{Owner: "yuri"}
		mut(&spec)
		return &agentsv1alpha1.AgentTask{Spec: spec}
	}

	tests := []struct {
		name    string
		task    *agentsv1alpha1.AgentTask
		wantErr string
		wantDef bool
	}{
		{
			name: "profile with prompt",
			task: task(func(s *agentsv1alpha1.AgentTaskSpec) { s.Type = "researcher"; s.Prompt = "go" }),
		},
		{
			name:    "profile without prompt",
			task:    task(func(s *agentsv1alpha1.AgentTaskSpec) { s.Type = "researcher" }),
			wantErr: "prompt",
		},
		{
			name:    "inline without prompt is allowed",
			task:    task(func(s *agentsv1alpha1.AgentTaskSpec) { s.Definition = inlineDefinition }),
			wantDef: true,
		},
		{
			name: "both type and definition",
			task: task(func(s *agentsv1alpha1.AgentTaskSpec) {
				s.Type = "researcher"
				s.Prompt = "go"
				s.Definition = inlineDefinition
			}),
			wantErr: "exactly one",
		},
		{
			name:    "neither type nor definition",
			task:    task(func(s *agentsv1alpha1.AgentTaskSpec) { s.Prompt = "go" }),
			wantErr: "exactly one",
		},
		{
			name:    "invalid inline definition",
			task:    task(func(s *agentsv1alpha1.AgentTaskSpec) { s.Definition = "no frontmatter here" }),
			wantErr: "definition",
		},
		{
			name: "skills require inline mode",
			task: task(func(s *agentsv1alpha1.AgentTaskSpec) {
				s.Type = "researcher"
				s.Prompt = "go"
				s.Skills = []agentsv1alpha1.SkillRef{{Name: "one"}}
			}),
			wantErr: "skills",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, err := validateMode(tt.task)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q should contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantDef {
				if def == nil || def.Name != "worker" || def.Instruction != "You are a worker." {
					t.Fatalf("parsed definition wrong: %+v", def)
				}
			} else if def != nil {
				t.Fatalf("profile mode must not parse a definition, got %+v", def)
			}
		})
	}
}

func TestReconcileInlineCreatesPodWithBundle(t *testing.T) {
	sk := skill("summarize", nil, "Summarize findings.",
		agentmd.ToolGrant{Name: "http", AllowHosts: []string{"example.com"}})
	sk.Namespace = "tasks"
	task := &agentsv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "tasks"},
		Spec: agentsv1alpha1.AgentTaskSpec{
			Owner:      "yuri",
			Definition: inlineDefinition,
			Skills:     []agentsv1alpha1.SkillRef{{Name: "summarize"}},
		},
	}

	r, c := newFakeReconciler(t, &sk, task)

	key := types.NamespacedName{Name: "t1", Namespace: "tasks"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "at-t1", Namespace: "agents"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}

	env := pod.Spec.Containers[0].Env
	defEnv, ok := envValue(env, "AGENT_BUNDLE_DEFINITION")
	if !ok || !strings.Contains(defEnv, `"name":"worker"`) {
		t.Fatalf("AGENT_BUNDLE_DEFINITION missing/wrong: %q", defEnv)
	}
	instr, ok := envValue(env, "AGENT_BUNDLE_INSTRUCTION")
	if !ok || !strings.Contains(instr, "You are a worker.") || !strings.Contains(instr, "## Skill: summarize") {
		t.Fatalf("AGENT_BUNDLE_INSTRUCTION missing skill section: %q", instr)
	}
	toolsEnv, ok := envValue(env, "AGENT_BUNDLE_TOOLS")
	if !ok || !strings.Contains(toolsEnv, `"http"`) || !strings.Contains(toolsEnv, `"exec"`) {
		t.Fatalf("AGENT_BUNDLE_TOOLS missing merged grants: %q", toolsEnv)
	}
	hash, ok := envValue(env, "AGENT_BUNDLE_HASH")
	if !ok || hash == "" {
		t.Fatal("AGENT_BUNDLE_HASH missing")
	}
	if got := pod.Annotations["agents.yurifrl.dev/bundle-hash"]; got != hash {
		t.Fatalf("bundle-hash annotation %q != env hash %q", got, hash)
	}

	for _, a := range pod.Spec.Containers[0].Args {
		if a == "--profile" {
			t.Fatalf("inline mode must not pass --profile, args: %v", pod.Spec.Containers[0].Args)
		}
	}
	if _, ok := envValue(env, "AGENT_PROFILE"); ok {
		t.Fatal("inline mode must not set AGENT_PROFILE")
	}
	if pod.Labels["agents.yurifrl.dev/type"] != "worker" {
		t.Fatalf("type label = %q, want worker (definition name)", pod.Labels["agents.yurifrl.dev/type"])
	}
}

func TestReconcileProfileModeRegression(t *testing.T) {
	task := &agentsv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t2", Namespace: "tasks"},
		Spec: agentsv1alpha1.AgentTaskSpec{
			Owner:  "yuri",
			Type:   "researcher",
			Prompt: "research things",
		},
	}

	r, c := newFakeReconciler(t, task)

	key := types.NamespacedName{Name: "t2", Namespace: "tasks"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "at-t2", Namespace: "agents"}, &pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}

	args := pod.Spec.Containers[0].Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--profile researcher") {
		t.Fatalf("profile mode must pass --profile researcher, args: %v", args)
	}
	env := pod.Spec.Containers[0].Env
	if v, _ := envValue(env, "AGENT_PROFILE"); v != "researcher" {
		t.Fatalf("AGENT_PROFILE = %q, want researcher", v)
	}
	for _, name := range []string{"AGENT_BUNDLE_DEFINITION", "AGENT_BUNDLE_INSTRUCTION", "AGENT_BUNDLE_TOOLS", "AGENT_BUNDLE_HASH"} {
		if _, ok := envValue(env, name); ok {
			t.Fatalf("profile mode must not set %s", name)
		}
	}
	if _, ok := pod.Annotations["agents.yurifrl.dev/bundle-hash"]; ok {
		t.Fatal("profile mode must not set the bundle-hash annotation")
	}
	if got := pod.Spec.Containers[0].Image; got != "ghcr.io/yurifrl/agents:cfg" {
		t.Fatalf("image = %q, want configured default", got)
	}
	if got := pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name; got != "agents" {
		t.Fatalf("envFrom secret = %q, want configured default", got)
	}
	if pod.Labels["agents.yurifrl.dev/type"] != "researcher" {
		t.Fatalf("type label = %q, want researcher", pod.Labels["agents.yurifrl.dev/type"])
	}
}

func TestReconcileInvalidSpecFailsTask(t *testing.T) {
	tests := []struct {
		name string
		spec agentsv1alpha1.AgentTaskSpec
	}{
		{
			name: "invalid inline definition",
			spec: agentsv1alpha1.AgentTaskSpec{Owner: "yuri", Definition: "no frontmatter"},
		},
		{
			name: "both type and definition",
			spec: agentsv1alpha1.AgentTaskSpec{Owner: "yuri", Type: "researcher", Prompt: "go", Definition: inlineDefinition},
		},
		{
			name: "profile without prompt",
			spec: agentsv1alpha1.AgentTaskSpec{Owner: "yuri", Type: "researcher"},
		},
		{
			name: "missing skill",
			spec: agentsv1alpha1.AgentTaskSpec{Owner: "yuri", Definition: inlineDefinition,
				Skills: []agentsv1alpha1.SkillRef{{Name: "ghost"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &agentsv1alpha1.AgentTask{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "tasks"},
				Spec:       tt.spec,
			}
			r, c := newFakeReconciler(t, task)

			key := types.NamespacedName{Name: "bad", Namespace: "tasks"}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			var got agentsv1alpha1.AgentTask
			if err := c.Get(context.Background(), key, &got); err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status.Phase != agentsv1alpha1.AgentTaskPhaseFailed {
				t.Fatalf("phase = %q, want Failed (msg %q)", got.Status.Phase, got.Status.Message)
			}

			var pod corev1.Pod
			err := c.Get(context.Background(), types.NamespacedName{Name: "at-bad", Namespace: "agents"}, &pod)
			if err == nil {
				t.Fatal("no pod must be created for an invalid task")
			}
		})
	}
}

func TestBuildPodExecutionFields(t *testing.T) {
	def, err := agentmd.Parse(inlineDefinition)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bundle := Compose(def, nil)

	t.Run("overrides win", func(t *testing.T) {
		task := &agentsv1alpha1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Name: "t3", Namespace: "tasks"},
			Spec: agentsv1alpha1.AgentTaskSpec{
				Owner:              "yuri",
				Definition:         inlineDefinition,
				Image:              "custom/image:v1",
				ServiceAccountName: "task-sa",
				SecretName:         "task-secret",
				Env:                []corev1.EnvVar{{Name: "EXTRA", Value: "1"}},
			},
		}
		r := NewReconciler(nil, testConfig())
		pod := r.buildPod(task, bundle)

		if got := pod.Spec.Containers[0].Image; got != "custom/image:v1" {
			t.Fatalf("image = %q", got)
		}
		if got := pod.Spec.ServiceAccountName; got != "task-sa" {
			t.Fatalf("serviceAccountName = %q", got)
		}
		if got := pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name; got != "task-secret" {
			t.Fatalf("envFrom secret = %q", got)
		}
		if v, ok := envValue(pod.Spec.Containers[0].Env, "EXTRA"); !ok || v != "1" {
			t.Fatalf("EXTRA env missing: %v", pod.Spec.Containers[0].Env)
		}
	})

	t.Run("configured defaults preserved", func(t *testing.T) {
		task := &agentsv1alpha1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Name: "t4", Namespace: "tasks"},
			Spec: agentsv1alpha1.AgentTaskSpec{
				Owner:      "yuri",
				Definition: inlineDefinition,
			},
		}
		r := NewReconciler(nil, testConfig())
		pod := r.buildPod(task, bundle)

		if got := pod.Spec.Containers[0].Image; got != "ghcr.io/yurifrl/agents:cfg" {
			t.Fatalf("image = %q", got)
		}
		if got := pod.Spec.ServiceAccountName; got != "" {
			t.Fatalf("serviceAccountName = %q, want empty default", got)
		}
		if got := pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name; got != "agents" {
			t.Fatalf("envFrom secret = %q", got)
		}
	})
}
