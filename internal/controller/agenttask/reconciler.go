// Package agenttask reconciles AgentTask resources into ephemeral agent pods.
package agenttask

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/config"
)

//+kubebuilder:rbac:groups=agents.yurifrl.dev,resources=agenttasks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.yurifrl.dev,resources=agenttasks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.yurifrl.dev,resources=agenttasks/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconciler reconciles ephemeral AgentTask resources into Pod (+Service).
type Reconciler struct {
	client client.Client
	cfg    *config.Config
}

// NewReconciler builds an AgentTask reconciler.
func NewReconciler(c client.Client, cfg *config.Config) *Reconciler {
	return &Reconciler{client: c, cfg: cfg}
}

// SetupWithManager registers the reconciler and its owned resources.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentTask{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// Reconcile drives an AgentTask toward its desired state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var task agentsv1alpha1.AgentTask
	if err := r.client.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Owner references handle GC; nothing to do on delete for the MVP.
	if !task.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if task.Spec.Lifecycle == agentsv1alpha1.LifecyclePersistent {
		// Persistent (Deployment-backed) lifecycle is a later milestone.
		logger.Info("persistent lifecycle not yet implemented; skipping", "task", task.Name)
		return ctrl.Result{}, nil
	}

	// Terminal tasks are left alone.
	switch task.Status.Phase {
	case agentsv1alpha1.AgentTaskPhaseSucceeded,
		agentsv1alpha1.AgentTaskPhaseFailed,
		agentsv1alpha1.AgentTaskPhaseCanceled:
		return ctrl.Result{}, nil
	}

	// Max-concurrency guard (ephemeral only): if at capacity and this task has no
	// pod yet, hold it Pending and requeue.
	if task.Status.PodName == "" && r.cfg.MaxConcurrent > 0 {
		atCapacity, err := r.atCapacity(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if atCapacity {
			if _, err := r.setPhase(ctx, &task, agentsv1alpha1.AgentTaskPhasePending,
				fmt.Sprintf("waiting for capacity (max %d)", r.cfg.MaxConcurrent)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	pod, err := r.ensurePod(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		// Restart budget exhausted; mark failed.
		_, err := r.setPhase(ctx, &task, agentsv1alpha1.AgentTaskPhaseFailed,
			fmt.Sprintf("pod failed and restart budget (%d) exhausted", r.cfg.RestartBudget))
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, &task); err != nil {
		return ctrl.Result{}, err
	}

	return r.syncStatus(ctx, &task, pod)
}

// atCapacity reports whether the number of live managed pods meets the limit.
func (r *Reconciler) atCapacity(ctx context.Context) (bool, error) {
	var pods corev1.PodList
	if err := r.client.List(ctx, &pods,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "agents-controller"},
	); err != nil {
		return false, err
	}
	live := 0
	for i := range pods.Items {
		switch pods.Items[i].Status.Phase {
		case corev1.PodPending, corev1.PodRunning:
			live++
		}
	}
	return live >= r.cfg.MaxConcurrent, nil
}

// setPhase patches only the status phase/message.
func (r *Reconciler) setPhase(ctx context.Context, task *agentsv1alpha1.AgentTask, phase agentsv1alpha1.AgentTaskPhase, msg string) (ctrl.Result, error) {
	if task.Status.Phase == phase && task.Status.Message == msg {
		return ctrl.Result{}, nil
	}
	updated := task.DeepCopy()
	updated.Status.Phase = phase
	updated.Status.Message = msg
	if err := r.client.Status().Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return ctrl.Result{}, nil
}

// ensurePod creates the agent pod if missing and returns the live pod. If the
// pod has failed it is recreated until the restart budget is exhausted, in which
// case ensurePod returns (nil, nil) to signal terminal failure.
func (r *Reconciler) ensurePod(ctx context.Context, task *agentsv1alpha1.AgentTask) (*corev1.Pod, error) {
	key := types.NamespacedName{Name: podName(task), Namespace: r.cfg.Namespace}

	var existing corev1.Pod
	err := r.client.Get(ctx, key, &existing)
	if err == nil {
		if existing.Status.Phase != corev1.PodFailed {
			return &existing, nil
		}
		// Pod failed: recreate within budget, else signal terminal failure.
		if task.Status.Restarts >= r.cfg.RestartBudget {
			return nil, nil
		}
		if err := r.client.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting failed pod: %w", err)
		}
		updated := task.DeepCopy()
		updated.Status.Restarts++
		if err := r.client.Status().Update(ctx, updated); err != nil {
			return nil, fmt.Errorf("bumping restart count: %w", err)
		}
		task.Status.Restarts = updated.Status.Restarts
		// Recreate below.
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting pod: %w", err)
	}

	pod := r.buildPod(task)
	if err := controllerutil.SetControllerReference(task, pod, r.client.Scheme()); err != nil {
		return nil, fmt.Errorf("setting pod owner ref: %w", err)
	}
	if err := r.client.Create(ctx, pod); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return pod, nil
		}
		return nil, fmt.Errorf("creating pod: %w", err)
	}
	return pod, nil
}

// ensureService creates the task Service if missing.
func (r *Reconciler) ensureService(ctx context.Context, task *agentsv1alpha1.AgentTask) error {
	key := types.NamespacedName{Name: serviceName(task), Namespace: r.cfg.Namespace}

	var existing corev1.Service
	err := r.client.Get(ctx, key, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting service: %w", err)
	}

	svc := r.buildService(task)
	if err := controllerutil.SetControllerReference(task, svc, r.client.Scheme()); err != nil {
		return fmt.Errorf("setting service owner ref: %w", err)
	}
	if err := r.client.Create(ctx, svc); err != nil {
		return fmt.Errorf("creating service: %w", err)
	}
	return nil
}

// syncStatus maps pod phase onto AgentTask status.
func (r *Reconciler) syncStatus(ctx context.Context, task *agentsv1alpha1.AgentTask, pod *corev1.Pod) (ctrl.Result, error) {
	updated := task.DeepCopy()
	updated.Status.PodName = pod.Name
	updated.Status.ObservedGeneration = task.Generation

	switch pod.Status.Phase {
	case corev1.PodPending:
		updated.Status.Phase = agentsv1alpha1.AgentTaskPhasePending
		updated.Status.Message = "pod pending"
	case corev1.PodRunning:
		updated.Status.Phase = agentsv1alpha1.AgentTaskPhaseRunning
		updated.Status.Message = "pod running"
		if updated.Status.StartedAt == nil {
			now := metav1.Now()
			updated.Status.StartedAt = &now
		}
	case corev1.PodSucceeded:
		updated.Status.Phase = agentsv1alpha1.AgentTaskPhaseSucceeded
		updated.Status.Message = "pod completed"
		if updated.Status.CompletedAt == nil {
			now := metav1.Now()
			updated.Status.CompletedAt = &now
		}
	case corev1.PodFailed:
		updated.Status.Phase = agentsv1alpha1.AgentTaskPhaseFailed
		updated.Status.Message = "pod failed"
		if updated.Status.CompletedAt == nil {
			now := metav1.Now()
			updated.Status.CompletedAt = &now
		}
	default:
		updated.Status.Phase = agentsv1alpha1.AgentTaskPhasePending
	}

	if equalStatus(task.Status, updated.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.client.Status().Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return ctrl.Result{}, nil
}

func equalStatus(a, b agentsv1alpha1.AgentTaskStatus) bool {
	return a.Phase == b.Phase &&
		a.PodName == b.PodName &&
		a.Message == b.Message &&
		a.ObservedGeneration == b.ObservedGeneration
}
