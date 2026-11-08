package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yurifrl/agents/internal/agentmd"
)

// AgentSkillSpec is a reusable bundle of instructions and tool policy that can
// be composed into an agent definition.
type AgentSkillSpec struct {
	// Description is a short human-readable summary of the skill.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Description string `json:"description"`

	// Instructions is the markdown body appended to the agent instruction as a
	// "## Skill: <name>" section during composition.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Instructions string `json:"instructions"`

	// Tools are tool grants contributed by this skill. Grants for the same tool
	// are merged across the definition and all skills (union of allow rules and
	// hosts, strictest non-zero limits).
	// +optional
	Tools []agentmd.ToolGrant `json:"tools,omitempty"`
}

// AgentSkillStatus defines the observed state of an AgentSkill.
type AgentSkillStatus struct {
	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ask
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentSkill is a reusable, composable unit of agent instructions and tool policy.
type AgentSkill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSkillSpec   `json:"spec,omitempty"`
	Status AgentSkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentSkillList contains a list of AgentSkill.
type AgentSkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSkill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSkill{}, &AgentSkillList{})
}
