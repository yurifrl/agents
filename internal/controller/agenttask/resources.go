package agenttask

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
)

const (
	// AgentPort is the port the in-pod worker bridge listens on (omp RPC over
	// TCP; the supervisor dials the task Service).
	AgentPort = 8080

	// labelTask and labelType identify the task and agent type on pods/services.
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelTask      = "agents.yurifrl.dev/task"
	labelType      = "agents.yurifrl.dev/type"

	// annotationBundleHash records the resolved bundle content hash.
	annotationBundleHash = "agents.yurifrl.dev/bundle-hash"

	// repoVolume is the emptyDir shared by the clone init container and the
	// agent container when spec.repoSpec is set.
	repoVolume = "repo"

	// defaultClonePath matches the RepoSpec CEL default.
	defaultClonePath = "/workspace/repo"
)

// podName derives a stable pod name from the task.
func podName(task *agentsv1alpha1.AgentTask) string {
	return fmt.Sprintf("at-%s", task.Name)
}

// serviceName derives a stable service name from the task.
func serviceName(task *agentsv1alpha1.AgentTask) string {
	return fmt.Sprintf("at-%s", task.Name)
}

// labelsFor returns the identifying labels for resources owned by a task. The
// type label comes from spec.Type in profile mode; buildPod overwrites it with
// the definition name in inline mode.
func labelsFor(task *agentsv1alpha1.AgentTask) map[string]string {
	return map[string]string{
		labelManagedBy:              "agents-controller",
		labelTask:                   task.Name,
		labelType:                   task.Spec.Type,
		"app.kubernetes.io/part-of": "agents",
	}
}

// cloneArgs builds the git clone argv for a RepoSpec.
func cloneArgs(repo *agentsv1alpha1.RepoSpec) []string {
	args := []string{"clone", "--depth", "1"}
	if repo.Ref != "" {
		args = append(args, "--branch", repo.Ref)
	}
	path := repo.ClonePath
	if path == "" {
		path = defaultClonePath
	}
	return append(args, repo.URL, path)
}

// buildPod renders the ephemeral agent pod for a task. The pod runs the
// `worker` entrypoint (omp RPC bridged over TCP), mounts the shared agents
// secret wholesale, and is bounded by an activeDeadlineSeconds hard ceiling.
// bundle is the composed inline-mode bundle (nil in profile mode).
func (r *Reconciler) buildPod(task *agentsv1alpha1.AgentTask, bundle *ResolvedBundle) *corev1.Pod {
	deadline := r.cfg.DefaultDeadlineSeconds
	if task.Spec.ActiveDeadlineSeconds != nil {
		deadline = *task.Spec.ActiveDeadlineSeconds
	}

	labels := labelsFor(task)
	env := []corev1.EnvVar{
		{Name: "AGENT_TASK_NAME", Value: task.Name},
		{Name: "AGENT_OWNER", Value: task.Spec.Owner},
	}
	annotations := map[string]string{}
	args := []string{"worker"}

	if bundle != nil {
		// Inline mode: carry the deterministic bundle into the pod.
		defJSON, _ := json.Marshal(bundle.Definition)
		grantsJSON, _ := json.Marshal(bundle.Grants)
		env = append(env,
			corev1.EnvVar{Name: "AGENT_BUNDLE_DEFINITION", Value: string(defJSON)},
			corev1.EnvVar{Name: "AGENT_BUNDLE_INSTRUCTION", Value: bundle.Instruction},
			corev1.EnvVar{Name: "AGENT_BUNDLE_TOOLS", Value: string(grantsJSON)},
			corev1.EnvVar{Name: "AGENT_BUNDLE_HASH", Value: bundle.Hash},
		)
		annotations[annotationBundleHash] = bundle.Hash
		if bundle.Definition != nil && bundle.Definition.Name != "" {
			labels[labelType] = bundle.Definition.Name
		}
	} else {
		// Profile mode.
		env = append(env, corev1.EnvVar{Name: "AGENT_PROFILE", Value: task.Spec.Type})
		args = append(args, "--profile", task.Spec.Type)
	}

	// RepoSpec pre-clone: an init container clones the repo into a shared
	// emptyDir before the agent starts; the agent works from the clone.
	var initContainers []corev1.Container
	volumes := []corev1.Volume{}
	if task.Spec.RepoSpec != nil {
		repo := task.Spec.RepoSpec
		path := repo.ClonePath
		if path == "" {
			path = defaultClonePath
		}
		cloneImage := r.cfg.CloneImage
		if cloneImage == "" {
			cloneImage = r.cfg.AgentImage
		}
		initContainers = append(initContainers, corev1.Container{
			Name:  "clone",
			Image: cloneImage,
			Args:  cloneArgs(repo),
			EnvFrom: []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: r.secretName(task)},
				},
			}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      repoVolume,
				MountPath: path,
			}},
		})
		volumes = append(volumes, corev1.Volume{
			Name: repoVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		env = append(env, corev1.EnvVar{Name: "REMOTE_AGENTS_WORKDIR", Value: path})
	}

	agent := corev1.Container{
		Name:  "agent",
		Image: r.imageFor(task),
		Args:  args,
		Ports: []corev1.ContainerPort{
			{Name: "rpc", ContainerPort: AgentPort, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt(AgentPort),
				},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       3,
		},
		Env:     env,
		EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: r.secretName(task)}}}},
	}
	if task.Spec.RepoSpec != nil {
		path := task.Spec.RepoSpec.ClonePath
		if path == "" {
			path = defaultClonePath
		}
		agent.VolumeMounts = []corev1.VolumeMount{{Name: repoVolume, MountPath: path}}
	}
	agent.Env = append(agent.Env, task.Spec.Env...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName(task),
			Namespace:   r.cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:    task.Spec.ServiceAccountName,
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadline,
			ImagePullSecrets:      r.imagePullSecrets(),
			InitContainers:        initContainers,
			Containers:            []corev1.Container{agent},
			Volumes:               volumes,
		},
	}
}

// imageFor resolves the task image override against the configured default.
func (r *Reconciler) imageFor(task *agentsv1alpha1.AgentTask) string {
	if task.Spec.Image != "" {
		return task.Spec.Image
	}
	return r.cfg.AgentImage
}

// secretName resolves the task secret override against the configured default.
func (r *Reconciler) secretName(task *agentsv1alpha1.AgentTask) string {
	if task.Spec.SecretName != "" {
		return task.Spec.SecretName
	}
	return r.cfg.SecretName
}

// imagePullSecrets returns the configured pull secret reference, if any.
func (r *Reconciler) imagePullSecrets() []corev1.LocalObjectReference {
	if r.cfg.ImagePullSecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: r.cfg.ImagePullSecret}}
}

// buildService renders the Service in front of the pod so the supervisor can
// reach the in-pod worker bridge.
func (r *Reconciler) buildService(task *agentsv1alpha1.AgentTask) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(task),
			Namespace: r.cfg.Namespace,
			Labels:    labelsFor(task),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{labelTask: task.Name},
			Ports: []corev1.ServicePort{
				{
					Name:       "rpc",
					Port:       AgentPort,
					TargetPort: intstr.FromInt(AgentPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
