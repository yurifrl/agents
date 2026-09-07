package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Output is the externalization contract for a finished run.
// +kubebuilder:validation:Enum=response;commit;pr
type Output string

const (
	// OutputResponse returns the agent's final text (no git side effects).
	OutputResponse Output = "response"
	// OutputCommit pushes a branch with the changes but opens no PR.
	OutputCommit Output = "commit"
	// OutputPR pushes a branch and opens a pull request.
	OutputPR Output = "pr"
)

// Runtime selects how the agent is executed inside the pod.
// +kubebuilder:validation:Enum=adk;pi
type Runtime string

const (
	// RuntimeADK runs a generic Google ADK agent loop built from the Agent.md.
	RuntimeADK Runtime = "adk"
	// RuntimePi shells out to the pi CLI with the Agent.md body as system prompt.
	RuntimePi Runtime = "pi"
)

// Lifecycle controls whether the task is a one-shot pod or a warm deployment.
// +kubebuilder:validation:Enum=ephemeral;persistent
type Lifecycle string

const (
	// LifecycleEphemeral spawns a pod per task that dies when the task finishes.
	LifecycleEphemeral Lifecycle = "ephemeral"
	// LifecyclePersistent keeps the agent warm behind a stable Service.
	LifecyclePersistent Lifecycle = "persistent"
)

// AgentTaskPhase is the observed lifecycle phase of an AgentTask.
type AgentTaskPhase string

const (
	AgentTaskPhasePending   AgentTaskPhase = "Pending"
	AgentTaskPhaseRunning   AgentTaskPhase = "Running"
	AgentTaskPhaseSucceeded AgentTaskPhase = "Succeeded"
	AgentTaskPhaseFailed    AgentTaskPhase = "Failed"
	AgentTaskPhaseCanceled  AgentTaskPhase = "Canceled"
)

// SkillRef selects one AgentSkill by name or a set of AgentSkills by label
// selector. Exactly one of Name or Selector must be set.
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.selector)",message="exactly one of name or selector must be set"
type SkillRef struct {
	// Name selects a single AgentSkill in the task namespace. A missing skill
	// fails validation before any workload is created.
	// +optional
	Name string `json:"name,omitempty"`

	// Selector selects all matching AgentSkills in the task namespace. An empty
	// result is valid and contributes zero skills.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// AgentTaskSpec defines the desired state of an AgentTask.
// +kubebuilder:validation:XValidation:rule="has(self.type) != has(self.definition)",message="exactly one of type or definition must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || has(self.prompt)",message="prompt is required when type (profile mode) is set"
type AgentTaskSpec struct {
	// Type is the agent profile/definition name to run (e.g. "researcher", "worker").
	// It selects which Agent.md the entrypoint loads. Mutually exclusive with
	// Definition (profile mode).
	// +optional
	Type string `json:"type,omitempty"`

	// Definition is an inline Agent.md document (frontmatter + body) that fully
	// describes the agent. Mutually exclusive with Type (inline mode).
	// +optional
	Definition string `json:"definition,omitempty"`

	// Prompt is the task instruction handed to the agent. Required in profile
	// mode; optional in inline mode.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// Skills selects AgentSkill resources composed into the agent bundle.
	// Inline mode only.
	// +optional
	Skills []SkillRef `json:"skills,omitempty"`

	// Image overrides the controller's configured agent image for this task.
	// +optional
	Image string `json:"image,omitempty"`

	// ServiceAccountName is the pod service account. Empty uses the namespace
	// default.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// SecretName overrides the controller's configured secret mounted wholesale
	// into the agent pod.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Env adds extra environment variables to the agent container, after the
	// built-ins.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Repo is the git repository the agent operates on (for pr/commit outputs).
	// Accepts "owner/repo" or a full https URL. Optional for response-only agents.
	// +optional
	Repo string `json:"repo,omitempty"`

	// Output is the externalization contract: response (default), commit, or pr.
	// +kubebuilder:default=response
	// +optional
	Output Output `json:"output,omitempty"`

	// Owner is the durable identity that owns this task (e.g. "yuri").
	// Reconciliation and listing are keyed by owner, not by pi session.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// Runtime selects the execution engine. Defaults to adk.
	// +kubebuilder:default=adk
	// +optional
	Runtime Runtime `json:"runtime,omitempty"`

	// Lifecycle selects ephemeral (per-task pod) vs persistent (warm deployment).
	// Defaults to ephemeral.
	// +kubebuilder:default=ephemeral
	// +optional
	Lifecycle Lifecycle `json:"lifecycle,omitempty"`

	// ActiveDeadlineSeconds is the hard ceiling after which an ephemeral pod is
	// killed regardless of progress. Ignored for persistent lifecycle.
	// +kubebuilder:validation:Minimum=30
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
}

// AgentTaskStatus defines the observed state of an AgentTask.
type AgentTaskStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase AgentTaskPhase `json:"phase,omitempty"`

	// PodName is the name of the pod backing this task.
	// +optional
	PodName string `json:"podName,omitempty"`

	// Artifacts are the externalized results (PR URLs, pushed branches, text refs).
	// +optional
	Artifacts []string `json:"artifacts,omitempty"`

	// Restarts counts how many times a crashed pod has been recreated.
	// +optional
	Restarts int32 `json:"restarts,omitempty"`

	// Message is a human-readable detail about the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the backing pod first started running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the task reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest observations of the task state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=at
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentTask is a single unit of remote agent work reconciled into a Pod (+Service).
type AgentTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentTaskSpec   `json:"spec,omitempty"`
	Status AgentTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentTaskList contains a list of AgentTask.
type AgentTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentTask{}, &AgentTaskList{})
}
