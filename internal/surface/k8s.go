package surface

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yurifrl/agents/internal/protocol"
)

// K8sSurface runs workers as in-cluster pods: the agent image's `agents
// worker` bridge listens on WorkerPort, and this surface waits for the pod to
// become ready and dials it over the pod IP. Pods keep running across server
// restarts — re-adoption dials WorkerMeta Host:Port, exactly like the herdr
// surface's bridge reconnect.
type K8sSurface struct {
	Client     client.Client
	Namespace  string
	Image      string
	SecretName string
	// DialBudget bounds the wait for a pod to become Running and dialable
	// (image pull included). Defaults to 3m.
	DialBudget time.Duration
	// Poll is the pod-phase poll interval. Defaults to 500ms.
	Poll time.Duration
	// Port is the bridge port both sides use (--port for `agents worker`).
	// Zero defaults to WorkerPort.
	Port int
}

// WorkerPort is the port the in-pod worker bridge listens on (matches the
// controller's AgentTask service port).
const WorkerPort = 8080

// resourceName turns a task id into a valid pod/service name.
var resourceNameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

func resourceName(taskID string) string {
	name := strings.ToLower(taskID)
	name = resourceNameInvalid.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "task"
	}
	if len(name) > 57 { // leave room for the "ra-" prefix within 63 chars
		name = name[:57]
	}
	return "ra-" + name
}

// NewK8sSurface builds the k8s surface with default timing.
func NewK8sSurface(c client.Client, namespace, image, secretName string) *K8sSurface {
	return &K8sSurface{
		Client:     c,
		Namespace:  namespace,
		Image:      image,
		SecretName: secretName,
		DialBudget: 3 * time.Minute,
		Poll:       50 * time.Millisecond,
	}
}

// Kind reports the surface kind.
func (s *K8sSurface) Kind() protocol.SurfaceKind { return protocol.SurfaceK8s }

// Start creates the worker Pod (+Service) and blocks until the bridge accepts
// TCP, then returns the connected channel.
func (s *K8sSurface) Start(spec WorkerSpec) (RunningWorker, error) {
	ctx := context.Background()
	port := s.Port
	if port == 0 {
		port = WorkerPort
	}
	name := resourceName(spec.TaskID)
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "agents-server",
		"app.kubernetes.io/name":       "agents-worker",
		"agents.yurifrl.dev/task":      sanitizeLabelValue(spec.TaskID),
	}

	env := []corev1.EnvVar{
		{Name: "REMOTE_AGENTS_SESSION_DIR", Value: "/workspace/sessions"},
		{Name: "REMOTE_AGENTS_WORKDIR", Value: "/workspace/work"},
		{Name: "REMOTE_AGENTS_MAX_TIME_S", Value: strconv.Itoa(spec.MaxTimeS)},
		{Name: TaskIDEnvVar, Value: spec.TaskID},
	}
	if spec.Model != "" {
		env = append(env, corev1.EnvVar{Name: "REMOTE_AGENTS_MODEL", Value: spec.Model})
	}
	var envFrom []corev1.EnvFromSource
	if s.SecretName != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretName}},
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// omp enforces its own wall clock; the pod deadline is the escape hatch.
			ActiveDeadlineSeconds: new(int64(spec.MaxTimeS + 60)),
			Containers: []corev1.Container{{
				Name:    "worker",
				Image:   s.Image,
				Command: []string{"agents"},
				Args:    []string{"worker", "--port", strconv.Itoa(port)},
				Ports: []corev1.ContainerPort{{
					Name: "rpc", ContainerPort: int32(port), Protocol: corev1.ProtocolTCP,
				}},
				Env:     env,
				EnvFrom: envFrom,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "sessions", MountPath: "/workspace/sessions"},
					{Name: "work", MountPath: "/workspace/work"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "sessions", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "rpc", Port: int32(port), TargetPort: intstr.FromInt(port), Protocol: corev1.ProtocolTCP,
			}},
		},
	}

	if err := s.Client.Create(ctx, svc); err != nil && !k8serrors.IsAlreadyExists(err) {
		return RunningWorker{}, fmt.Errorf("creating service %s: %w", name, err)
	}
	if err := s.Client.Create(ctx, pod); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return RunningWorker{}, fmt.Errorf("creating pod %s: %w", name, err)
		}
	}

	conn, err := s.dialWhenReady(ctx, name, port)
	if err != nil {
		s.destroy(name)
		return RunningWorker{}, err
	}

	return RunningWorker{
		Channel: conn,
		Meta:    protocol.TaskWorkerMeta{Port: &port, Host: conn.RemoteAddr().(*net.TCPAddr).IP.String()},
		Stop:    func() { s.destroy(name) },
	}, nil
}

// dialWhenReady polls the pod until it is Running with an IP, then dials the
// bridge port. A pod that reaches a terminal phase fails fast.
func (s *K8sSurface) dialWhenReady(ctx context.Context, name string, port int) (net.Conn, error) {
	deadline := time.Now().Add(s.DialBudget)
	var lastErr error
	for time.Now().Before(deadline) {
		var pod corev1.Pod
		err := s.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: s.Namespace}, &pod)
		if err != nil {
			return nil, fmt.Errorf("getting pod %s: %w", name, err)
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, fmt.Errorf("pod %s reached phase %s before accepting connections", name, pod.Status.Phase)
		}
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			conn, err := net.DialTimeout("tcp",
				net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), 2*time.Second)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		time.Sleep(s.Poll)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("pod %s bridge unreachable within %s: %w", name, s.DialBudget, lastErr)
	}
	return nil, fmt.Errorf("pod %s not running within %s", name, s.DialBudget)
}

// destroy deletes the pod and service (best effort, with a short budget).
func (s *K8sSurface) destroy(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	_ = s.Client.Delete(ctx, pod)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	_ = s.Client.Delete(ctx, svc)
}

// sanitizeLabelValue makes a task id safe for label values (63 chars, alnum).
func sanitizeLabelValue(v string) string {
	out := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(v, "-")
	out = strings.Trim(out, "-._")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}
