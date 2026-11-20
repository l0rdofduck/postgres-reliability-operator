package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1 "github.com/l0rdofduck/postgres-reliability-operator/api/v1"
	"github.com/l0rdofduck/postgres-reliability-operator/internal/patroni"
)

func testCluster() *dbv1.PostgresCluster {
	return &dbv1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "pgc-demo", Generation: 1},
		Spec:       dbv1.PostgresClusterSpec{},
	}
}

func TestDefaults(t *testing.T) {
	pg := testCluster()
	if got := effectiveReplicas(pg); got != 3 {
		t.Fatalf("replicas default: got %d", got)
	}
	if got := effectiveImage(pg); got != defaultImage {
		t.Fatalf("image default: got %q", got)
	}
	if got := effectiveStorage(pg); got != defaultStorage {
		t.Fatalf("storage default: got %q", got)
	}
	p := effectivePatroni(pg)
	if p.TTL != 20 || p.LoopWait != 10 || p.RetryTimeout != 10 {
		t.Fatalf("unexpected patroni defaults: %+v", p)
	}
}

func TestNames(t *testing.T) {
	pg := testCluster()
	cases := map[string]string{
		configMapName(pg):       "demo-patroni-config",
		headlessServiceName(pg): "demo",
		primaryServiceName(pg):  "demo-primary",
		replicaServiceName(pg):  "demo-replicas",
		patroniRoleName(pg):     "demo-patroni",
		serviceAccountName(pg):  "demo",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("name mismatch: got %q want %q", got, want)
		}
	}
}

func TestPatroniConfig(t *testing.T) {
	pg := testCluster()
	pg.Spec.Patroni.TTL = 42
	cfg := patroniConfig(pg)

	for _, want := range []string{
		"scope: demo",
		"namespace: pgc-demo",
		"ttl: 42",
		"use_endpoints: false",
		"role_label: role",
		"data_dir: /home/postgres/pgdata/data",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("patroni config missing %q\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "PASSWORD") || strings.Contains(cfg, "password:") {
		t.Error("patroni config must not contain credentials; they are injected via env")
	}
}

func TestDesiredStatefulSet(t *testing.T) {
	pg := testCluster()
	pg.Spec.Replicas = 2
	pg.Spec.StorageSize = "2Gi"

	sts := desiredStatefulSet(pg)
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 {
		t.Fatalf("expected 2 replicas")
	}
	if sts.Spec.ServiceName != "demo" {
		t.Fatalf("unexpected service name %q", sts.Spec.ServiceName)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if c.Image != defaultImage {
		t.Fatalf("unexpected image %q", c.Image)
	}
	if len(c.Args) != 1 || c.Args[0] != "/config/patroni.yml" {
		t.Fatalf("unexpected args %v", c.Args)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for _, key := range []string{"PATRONI_SCOPE", "PATRONI_KUBERNETES_NAMESPACE", "PATRONI_RESTAPI_CONNECT_ADDRESS"} {
		if _, ok := env[key]; !ok {
			t.Errorf("missing env %s", key)
		}
	}

	pvc := sts.Spec.VolumeClaimTemplates[0]
	qty := pvc.Spec.Resources.Requests.Storage()
	if qty.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("unexpected storage request %s", qty.String())
	}
}

func TestDesiredServices(t *testing.T) {
	pg := testCluster()
	primary := desiredRoleService(pg, "demo-primary", "primary")
	if primary.Spec.Selector["role"] != "primary" {
		t.Fatalf("primary service selector: %+v", primary.Spec.Selector)
	}
	headless := desiredHeadlessService(pg)
	if headless.Spec.ClusterIP != "None" {
		t.Fatalf("expected headless service")
	}
	if !headless.Spec.PublishNotReadyAddresses {
		t.Fatal("headless service must publish not ready addresses for bootstrap")
	}
}

func TestToObserved(t *testing.T) {
	lag := int64(512)
	zero := int64(0)
	obs := toObserved(&patroni.Cluster{Members: []patroni.Member{
		{Name: "b", Role: "replica", State: "running", Lag: &zero},
		{Name: "a", Role: "leader", State: "running"},
		{Name: "c", Role: "replica", State: "streaming", Lag: &lag},
	}})
	if obs.Leader != "a" {
		t.Fatalf("expected leader a, got %q", obs.Leader)
	}
	if obs.InSync != 1 {
		t.Fatalf("expected 1 in-sync replica, got %d", obs.InSync)
	}
	if obs.Members[0].Name != "a" {
		t.Fatalf("members must be sorted, got %q first", obs.Members[0].Name)
	}
}

func TestSetStatusReady(t *testing.T) {
	pg := testCluster()
	r := &PostgresClusterReconciler{}
	obs := &observedCluster{
		Leader: "demo-0",
		InSync: 2,
		Members: []dbv1.ReplicaStatus{
			{Name: "demo-0", Role: "leader"},
			{Name: "demo-1", Role: "replica"},
			{Name: "demo-2", Role: "replica"},
		},
	}
	r.setStatus(pg, obs, nil)
	if pg.Status.Phase != dbv1.PhaseReady {
		t.Fatalf("expected Ready, got %q", pg.Status.Phase)
	}
	if got := len(pg.Status.Conditions); got != 3 {
		t.Fatalf("expected 3 conditions, got %d", got)
	}
}

func TestSetStatusDegraded(t *testing.T) {
	pg := testCluster()
	r := &PostgresClusterReconciler{}
	obs := &observedCluster{Leader: "demo-0", InSync: 0, Members: []dbv1.ReplicaStatus{{Name: "demo-0", Role: "leader"}}}
	r.setStatus(pg, obs, nil)
	if pg.Status.Phase != dbv1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %q", pg.Status.Phase)
	}
}

func TestFailoverTracker(t *testing.T) {
	var tr failoverTracker
	if _, changed := tr.observe("ns/demo", "demo-0"); changed {
		t.Fatal("first observation must not report a failover")
	}
	if _, changed := tr.observe("ns/demo", "demo-0"); changed {
		t.Fatal("same leader must not report a failover")
	}
	if _, changed := tr.observe("ns/demo", ""); changed {
		t.Fatal("missing leader must not report a failover")
	}
	d, changed := tr.observe("ns/demo", "demo-1")
	if !changed {
		t.Fatal("leader change must report a failover")
	}
	if d < 0 {
		t.Fatalf("failover duration must not be negative, got %v", d)
	}
	tr.forget("ns/demo")
	if _, changed := tr.observe("ns/demo", "demo-1"); changed {
		t.Fatal("after forget the tracker starts from scratch")
	}
}
