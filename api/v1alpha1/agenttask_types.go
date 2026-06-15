package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// AgentTaskSpec defines the desired state of an AgentTask.
type AgentTaskSpec struct {
	// Type is the agent profile/definition name to run (e.g. "researcher", "worker").
	// It selects which Agent.md the entrypoint loads.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Prompt is the task instruction handed to the agent.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Prompt string `json:"prompt"`

	// Repo is the git repository the agent operates on (for pr/commit outputs).
	// Optional for response-only agents.
	// +optional
	Repo string `json:"repo,omitempty"`

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
