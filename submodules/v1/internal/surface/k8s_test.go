package surface

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeK8s(t *testing.T, objs ...runtime.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	b := clientfake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return b.Build()
}

// freeBridgePort binds a listener to represent the in-pod bridge and returns
// its port; the listener stays open so the surface's dial succeeds.
func freeBridgePort(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bridge listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port, ln
}

func TestK8sSurfaceResourceName(t *testing.T) {
	got := resourceName("T-12_Foo/bar")
	if got != "ra-t-12-foo-bar" {
		t.Fatalf("resourceName = %q", got)
	}
	long := resourceName(strings.Repeat("x", 80))
	if len(long) > 63 || !strings.HasPrefix(long, "ra-") {
		t.Fatalf("long name invalid: %q (%d)", long, len(long))
	}
}

func TestK8sSurfaceStartDialsBridge(t *testing.T) {
	c := newFakeK8s(t)
	port, _ := freeBridgePort(t)
	s := NewK8sSurface(c, "agents", "ghcr.io/yurifrl/agents:latest", "agents")
	s.Port = port
	s.DialBudget = 5 * time.Second
	s.Poll = 10 * time.Millisecond

	done := make(chan RunningWorker, 1)
	errc := make(chan error, 1)
	go func() {
		w, err := s.Start(WorkerSpec{
			TaskID:     "t-1",
			SessionDir: "/tmp/s",
			OmpBin:     "omp",
			MaxTimeS:   60,
		})
		if err != nil {
			errc <- err
			return
		}
		done <- w
	}()

	// Flip the pod to Running with the bridge's IP once it exists.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pod corev1.Pod
		err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-1", Namespace: "agents"}, &pod)
		if err == nil {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.PodIP = "127.0.0.1"
			_ = c.Status().Update(context.Background(), &pod)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case w := <-done:
		if w.Meta.Host != "127.0.0.1" || w.Meta.Port == nil || *w.Meta.Port != port {
			t.Fatalf("meta = %+v", w.Meta)
		}
		if w.Meta.PID != nil {
			t.Fatal("k8s workers must not report a pid")
		}
		if _, err := w.Channel.Write([]byte("ping\n")); err != nil {
			t.Fatalf("channel write: %v", err)
		}
		var svc corev1.Service
		if err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-1", Namespace: "agents"}, &svc); err != nil {
			t.Fatalf("service missing: %v", err)
		}
		w.Stop()
	case err := <-errc:
		t.Fatalf("Start: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Start timed out")
	}
}

func TestK8sSurfaceFailsFastOnDeadPod(t *testing.T) {
	c := newFakeK8s(t)
	s := NewK8sSurface(c, "agents", "img:latest", "")
	s.DialBudget = 5 * time.Second
	s.Poll = 10 * time.Millisecond

	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var pod corev1.Pod
			err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-dead", Namespace: "agents"}, &pod)
			if err == nil {
				pod.Status.Phase = corev1.PodFailed
				_ = c.Status().Update(context.Background(), &pod)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, err := s.Start(WorkerSpec{TaskID: "t-dead", SessionDir: "/tmp/s", MaxTimeS: 60})
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("expected fail-fast error, got %v", err)
	}
	// The dead pod's resources are cleaned up.
	if err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-dead", Namespace: "agents"}, &corev1.Pod{}); err == nil {
		t.Fatal("pod should have been deleted after failure")
	}
}

func TestK8sSurfacePodSpecShape(t *testing.T) {
	c := newFakeK8s(t)
	port, _ := freeBridgePort(t)
	s := NewK8sSurface(c, "agents", "img:latest", "agents")
	s.Port = port
	s.DialBudget = 5 * time.Second
	s.Poll = 10 * time.Millisecond

	flip := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var pod corev1.Pod
			err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-shape", Namespace: "agents"}, &pod)
			if err == nil {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "127.0.0.1"
				_ = c.Status().Update(context.Background(), &pod)
				close(flip)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	w, err := s.Start(WorkerSpec{TaskID: "t-shape", SessionDir: "/s", Model: "gpt", MaxTimeS: 120})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()
	<-flip

	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "ra-t-shape", Namespace: "agents"}, &pod); err != nil {
		t.Fatalf("pod: %v", err)
	}
	got := pod.Spec.Containers[0]
	if got.Image != "img:latest" {
		t.Fatalf("image = %q", got.Image)
	}
	if strings.Join(got.Command, " ") != "agents" || strings.Join(got.Args, " ") != "worker --port "+strconv.Itoa(port) {
		t.Fatalf("command/args = %v %v", got.Command, got.Args)
	}
	env := map[string]string{}
	for _, e := range got.Env {
		env[e.Name] = e.Value
	}
	if env["REMOTE_AGENTS_MODEL"] != "gpt" || env["REMOTE_AGENTS_MAX_TIME_S"] != "120" {
		t.Fatalf("env = %v", env)
	}
	if env["REMOTE_AGENTS_SESSION_DIR"] != "/workspace/sessions" || env["REMOTE_AGENTS_WORKDIR"] != "/workspace/work" {
		t.Fatalf("env = %v", env)
	}
	if len(got.EnvFrom) != 1 || got.EnvFrom[0].SecretRef.Name != "agents" {
		t.Fatalf("envFrom = %v", got.EnvFrom)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != int64(120+60) {
		t.Fatalf("deadline = %v", pod.Spec.ActiveDeadlineSeconds)
	}
}

// compile-time: the surface satisfies the Surface seam.
var _ Surface = (*K8sSurface)(nil)
