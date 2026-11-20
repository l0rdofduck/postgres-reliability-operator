package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"reflect"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1 "github.com/l0rdofduck/postgres-reliability-operator/api/v1"
	"github.com/l0rdofduck/postgres-reliability-operator/internal/metrics"
	"github.com/l0rdofduck/postgres-reliability-operator/internal/patroni"
)

const (
	requeueReady    = 5 * time.Second
	requeueNotReady = 3 * time.Second
)

// PostgresClusterReconciler reconciles a PostgresCluster object.
type PostgresClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	failovers failoverTracker
}

// +kubebuilder:rbac:groups=db.example.com,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.example.com,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;secrets;serviceaccounts;events;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the cluster towards the desired state.
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pg := &dbv1.PostgresCluster{}
	if err := r.Get(ctx, req.NamespacedName, pg); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.DeleteCluster(req.Namespace, req.Name)
			r.failovers.forget(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !pg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	steps := []struct {
		name string
		fn   func(context.Context, *dbv1.PostgresCluster) error
	}{
		{"serviceaccount", r.reconcileServiceAccount},
		{"rbac", r.reconcileRBAC},
		{"secret", r.reconcileSecret},
		{"configmap", r.reconcileConfigMap},
		{"headless-service", r.reconcileHeadlessService},
		{"role-services", r.reconcileRoleServices},
		{"statefulset", r.reconcileStatefulSet},
		{"pdb", r.reconcilePDB},
	}
	for _, step := range steps {
		if err := step.fn(ctx, pg); err != nil {
			log.Error(err, "reconcile step failed", "step", step.name)
			r.Recorder.Eventf(pg, corev1.EventTypeWarning, "ReconcileFailed", "%s: %v", step.name, err)
			return ctrl.Result{}, err
		}
	}

	observed, err := r.observe(ctx, pg)
	if err != nil {
		log.V(1).Info("patroni cluster not reachable yet", "reason", err.Error())
		r.setStatus(pg, nil, err)
	} else {
		r.setStatus(pg, observed, nil)
		r.trackFailover(pg, observed)
	}

	if err := r.Status().Update(ctx, pg); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if pg.Status.Phase == dbv1.PhaseReady {
		return ctrl.Result{RequeueAfter: requeueReady}, nil
	}
	return ctrl.Result{RequeueAfter: requeueNotReady}, nil
}

func (r *PostgresClusterReconciler) reconcileServiceAccount(ctx context.Context, pg *dbv1.PostgresCluster) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName(pg), Namespace: pg.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = labels(pg)
		return controllerutil.SetControllerReference(pg, sa, r.Scheme)
	})
	return err
}

func (r *PostgresClusterReconciler) reconcileRBAC(ctx context.Context, pg *dbv1.PostgresCluster) error {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: patroniRoleName(pg), Namespace: pg.Namespace}}
	desired := desiredPatroniRole(pg)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = desired.Labels
		role.Rules = desired.Rules
		return controllerutil.SetControllerReference(pg, role, r.Scheme)
	}); err != nil {
		return err
	}

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: patroniRoleName(pg), Namespace: pg.Namespace}}
	desiredBinding := desiredPatroniRoleBinding(pg)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = desiredBinding.Labels
		binding.RoleRef = desiredBinding.RoleRef
		binding.Subjects = desiredBinding.Subjects
		return controllerutil.SetControllerReference(pg, binding, r.Scheme)
	})
	return err
}

func (r *PostgresClusterReconciler) reconcileSecret(ctx context.Context, pg *dbv1.PostgresCluster) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: pg.Name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	superuser, err := randomPassword()
	if err != nil {
		return err
	}
	replication, err := randomPassword()
	if err != nil {
		return err
	}
	restapi, err := randomPassword()
	if err != nil {
		return err
	}

	secret := desiredSecret(pg, superuser, replication, restapi)
	if err := controllerutil.SetControllerReference(pg, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

func (r *PostgresClusterReconciler) reconcileConfigMap(ctx context.Context, pg *dbv1.PostgresCluster) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName(pg), Namespace: pg.Namespace}}
	desired := desiredConfigMap(pg)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = desired.Labels
		cm.Data = desired.Data
		return controllerutil.SetControllerReference(pg, cm, r.Scheme)
	})
	return err
}

func (r *PostgresClusterReconciler) reconcileHeadlessService(ctx context.Context, pg *dbv1.PostgresCluster) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(pg), Namespace: pg.Namespace}}
	desired := desiredHeadlessService(pg)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.ClusterIP = desired.Spec.ClusterIP
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(pg, svc, r.Scheme)
	})
	return err
}

func (r *PostgresClusterReconciler) reconcileRoleServices(ctx context.Context, pg *dbv1.PostgresCluster) error {
	for _, s := range []struct{ name, role string }{
		{primaryServiceName(pg), "primary"},
		{replicaServiceName(pg), "replica"},
	} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: pg.Namespace}}
		desired := desiredRoleService(pg, s.name, s.role)
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = desired.Labels
			svc.Spec.Selector = desired.Spec.Selector
			svc.Spec.Ports = desired.Spec.Ports
			return controllerutil.SetControllerReference(pg, svc, r.Scheme)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresClusterReconciler) reconcileStatefulSet(ctx context.Context, pg *dbv1.PostgresCluster) error {
	desired := desiredStatefulSet(pg)
	if err := controllerutil.SetControllerReference(pg, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: pg.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	changed := false
	for k, v := range desired.Labels {
		if existing.Labels[k] != v {
			existing.Labels[k] = v
			changed = true
		}
	}

	if existing.Spec.Replicas == nil || *existing.Spec.Replicas != *desired.Spec.Replicas {
		existing.Spec.Replicas = desired.Spec.Replicas
		changed = true
	}

	if len(existing.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		current := &existing.Spec.Template.Spec.Containers[0]
		next := desired.Spec.Template.Spec.Containers[0]
		if current.Image != next.Image || !equalEnv(current.Env, next.Env) {
			current.Image = next.Image
			current.Env = next.Env
			changed = true
		}
		if !reflect.DeepEqual(current.ReadinessProbe, next.ReadinessProbe) {
			current.ReadinessProbe = next.ReadinessProbe
			changed = true
		}
		if !reflect.DeepEqual(current.LivenessProbe, next.LivenessProbe) {
			current.LivenessProbe = next.LivenessProbe
			changed = true
		}
		if !reflect.DeepEqual(current.Resources, next.Resources) {
			current.Resources = next.Resources
			changed = true
		}
	}

	if !reflect.DeepEqual(existing.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
		changed = true
	}

	if !changed {
		return nil
	}
	return r.Update(ctx, existing)
}

// equalEnv compares two env lists, including valueFrom references.
func equalEnv(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
		if !reflect.DeepEqual(a[i].ValueFrom, b[i].ValueFrom) {
			return false
		}
	}
	return true
}

func (r *PostgresClusterReconciler) reconcilePDB(ctx context.Context, pg *dbv1.PostgresCluster) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: pg.Name, Namespace: pg.Namespace}}
	desired := desiredPDB(pg)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = desired.Labels
		pdb.Spec.MinAvailable = desired.Spec.MinAvailable
		pdb.Spec.Selector = desired.Spec.Selector
		return controllerutil.SetControllerReference(pg, pdb, r.Scheme)
	})
	return err
}

// observedCluster is the parsed view of the Patroni cluster.
type observedCluster struct {
	Leader  string
	Members []dbv1.ReplicaStatus
	InSync  int32
}

func (r *PostgresClusterReconciler) observe(ctx context.Context, pg *dbv1.PostgresCluster) (*observedCluster, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pg.Namespace, Name: pg.Name}, secret); err != nil {
		return nil, fmt.Errorf("load restapi credentials: %w", err)
	}
	password := string(secret.Data[secretRestapi])
	if password == "" {
		return nil, fmt.Errorf("restapi password is empty")
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(pg.Namespace), client.MatchingLabels(labels(pg))); err != nil {
		return nil, fmt.Errorf("list postgres pods: %w", err)
	}

	var lastErr error
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		baseURL := "http://" + net.JoinHostPort(pod.Status.PodIP, fmt.Sprintf("%d", patroniPort))
		pc := patroni.New(baseURL, "patroni", password, 2*time.Second)
		cluster, err := pc.Cluster(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		return toObserved(cluster), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no postgres pods with an IP yet")
	}
	return nil, lastErr
}

func toObserved(cluster *patroni.Cluster) *observedCluster {
	obs := &observedCluster{Members: make([]dbv1.ReplicaStatus, 0, len(cluster.Members))}
	for _, m := range cluster.Members {
		lag := int64(0)
		if m.Lag != nil {
			lag = *m.Lag
		}
		obs.Members = append(obs.Members, dbv1.ReplicaStatus{
			Name:     m.Name,
			Role:     m.Role,
			State:    m.State,
			LagBytes: lag,
		})
		if m.Role == "leader" || m.Role == "primary" {
			obs.Leader = m.Name
		} else if lag == 0 && (m.State == "running" || m.State == "streaming") {
			obs.InSync++
		}
	}
	sort.Slice(obs.Members, func(i, j int) bool { return obs.Members[i].Name < obs.Members[j].Name })
	return obs
}

func (r *PostgresClusterReconciler) setStatus(pg *dbv1.PostgresCluster, obs *observedCluster, observeErr error) {
	pg.Status.ObservedGeneration = pg.Generation

	ready := metav1.ConditionFalse
	leaderElected := metav1.ConditionFalse
	replicasInSync := metav1.ConditionFalse
	reason := "ClusterUnreachable"
	message := "Patroni API is not reachable yet"
	phase := dbv1.PhaseInitializing

	if obs != nil {
		if obs.Leader != "" {
			leaderElected = metav1.ConditionTrue
			reason = "LeaderElected"
		} else {
			reason = "NoLeader"
		}

		expectedReplicas := int(effectiveReplicas(pg)) - 1
		if expectedReplicas < 1 {
			expectedReplicas = 1
		}
		if int(obs.InSync) >= expectedReplicas {
			replicasInSync = metav1.ConditionTrue
		}

		if obs.Leader != "" && int32(len(obs.Members)) == effectiveReplicas(pg) && replicasInSync == metav1.ConditionTrue {
			ready = metav1.ConditionTrue
			phase = dbv1.PhaseReady
			reason = "ClusterReady"
			message = fmt.Sprintf("leader %s with %d in-sync replicas", obs.Leader, obs.InSync)
		} else {
			phase = dbv1.PhaseDegraded
			message = fmt.Sprintf("%d/%d members, %d in sync, leader %q",
				len(obs.Members), effectiveReplicas(pg), obs.InSync, obs.Leader)
		}

		pg.Status.Leader = obs.Leader
		pg.Status.Replicas = obs.Members
	} else {
		pg.Status.Leader = ""
		pg.Status.Replicas = nil
		if observeErr != nil {
			message = observeErr.Error()
		}
	}

	pg.Status.Phase = phase
	meta.SetStatusCondition(&pg.Status.Conditions, metav1.Condition{
		Type:               dbv1.ConditionReady,
		Status:             ready,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pg.Generation,
	})
	meta.SetStatusCondition(&pg.Status.Conditions, metav1.Condition{
		Type:               dbv1.ConditionLeaderElected,
		Status:             leaderElected,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pg.Generation,
	})
	meta.SetStatusCondition(&pg.Status.Conditions, metav1.Condition{
		Type:               dbv1.ConditionReplicasInSync,
		Status:             replicasInSync,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pg.Generation,
	})

	if obs != nil {
		metrics.ReplicasInSync.WithLabelValues(pg.Namespace, pg.Name).Set(float64(obs.InSync))
	}
	if obs != nil && obs.Leader != "" {
		metrics.LeaderHealthy.WithLabelValues(pg.Namespace, pg.Name).Set(1)
	} else {
		metrics.LeaderHealthy.WithLabelValues(pg.Namespace, pg.Name).Set(0)
	}
}

func (r *PostgresClusterReconciler) trackFailover(pg *dbv1.PostgresCluster, obs *observedCluster) {
	duration, changed := r.failovers.observe(pg.Namespace+"/"+pg.Name, obs.Leader)
	if !changed {
		return
	}
	metrics.FailoverSeconds.WithLabelValues(pg.Namespace, pg.Name).Set(duration.Seconds())
	r.Recorder.Eventf(pg, corev1.EventTypeNormal, "LeaderElected",
		"Leader changed to %s; observed failover window %.1fs", obs.Leader, duration.Seconds())
}

// SetupWithManager registers the controller.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Complete(r)
}

func randomPassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// failoverTracker measures the window between observing the old and the new
// leader. It is intentionally in-memory: after an operator restart the next
// leader change is measured from the first observation.
type failoverTracker struct {
	mu    sync.Mutex
	state map[string]failoverState
}

type failoverState struct {
	leader   string
	lastSeen time.Time
}

func (t *failoverTracker) observe(key, leader string) (time.Duration, bool) {
	if leader == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = map[string]failoverState{}
	}
	now := time.Now()
	s, ok := t.state[key]
	if !ok || s.leader == "" {
		t.state[key] = failoverState{leader: leader, lastSeen: now}
		return 0, false
	}
	if s.leader == leader {
		s.lastSeen = now
		t.state[key] = s
		return 0, false
	}
	duration := now.Sub(s.lastSeen)
	t.state[key] = failoverState{leader: leader, lastSeen: now}
	return duration, true
}

func (t *failoverTracker) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, key)
}
