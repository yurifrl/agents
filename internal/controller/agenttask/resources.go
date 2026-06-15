package agenttask

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
)

// AgentPort is the port the in-pod A2A server listens on.
const AgentPort = 8080

// podName derives a stable pod name from the task.
func podName(task *agentsv1alpha1.AgentTask) string {
	return fmt.Sprintf("at-%s", task.Name)
}

// serviceName derives a stable service name from the task.
func serviceName(task *agentsv1alpha1.AgentTask) string {
	return fmt.Sprintf("at-%s", task.Name)
}

// labelsFor returns the identifying labels for resources owned by a task.
func labelsFor(task *agentsv1alpha1.AgentTask) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "agents-controller",
		"agents.yurifrl.dev/task":      task.Name,
		"agents.yurifrl.dev/owner":     task.Spec.Owner,
		"agents.yurifrl.dev/type":      task.Spec.Type,
	}
}

// buildPod renders the ephemeral agent pod for a task. The pod runs
// agent-entrypoint, mounts the shared agents secret wholesale, and is bounded
// by an activeDeadlineSeconds hard ceiling.
func (r *Reconciler) buildPod(task *agentsv1alpha1.AgentTask) *corev1.Pod {
	deadline := r.cfg.DefaultDeadlineSeconds
	if task.Spec.ActiveDeadlineSeconds != nil {
		deadline = *task.Spec.ActiveDeadlineSeconds
	}

	runtime := task.Spec.Runtime
	if runtime == "" {
		runtime = agentsv1alpha1.RuntimeADK
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(task),
			Namespace: r.cfg.Namespace,
			Labels:    labelsFor(task),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadline,
			ImagePullSecrets:      r.imagePullSecrets(),
			Containers: []corev1.Container{
				{
					Name:  "agent",
					Image: r.cfg.AgentImage,
					Args: []string{
						"agent-entrypoint",
						"--profile", task.Spec.Type,
						"--runtime", string(runtime),
					},
					Ports: []corev1.ContainerPort{
						{Name: "a2a", ContainerPort: AgentPort, Protocol: corev1.ProtocolTCP},
					},
					Env: []corev1.EnvVar{
						{Name: "AGENT_TASK_NAME", Value: task.Name},
						{Name: "AGENT_OWNER", Value: task.Spec.Owner},
						{Name: "AGENT_PROMPT", Value: task.Spec.Prompt},
						{Name: "AGENT_REPO", Value: task.Spec.Repo},
					},
					EnvFrom: []corev1.EnvFromSource{
						{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: r.cfg.SecretName},
							},
						},
					},
				},
			},
		},
	}
}

// imagePullSecrets returns the configured pull secret reference, if any.
func (r *Reconciler) imagePullSecrets() []corev1.LocalObjectReference {
	if r.cfg.ImagePullSecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: r.cfg.ImagePullSecret}}
}

// buildService renders the headless-addressable Service in front of the pod so
// the gateway can reach the in-pod A2A server.
func (r *Reconciler) buildService(task *agentsv1alpha1.AgentTask) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(task),
			Namespace: r.cfg.Namespace,
			Labels:    labelsFor(task),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"agents.yurifrl.dev/task": task.Name},
			Ports: []corev1.ServicePort{
				{
					Name:       "a2a",
					Port:       AgentPort,
					TargetPort: intstr.FromInt(AgentPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
