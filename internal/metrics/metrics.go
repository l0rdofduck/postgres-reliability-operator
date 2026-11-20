// Package metrics exposes Prometheus metrics for the operator.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// LeaderHealthy is 1 when the cluster reports a leader, 0 otherwise.
	LeaderHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "pgc",
		Name:      "leader_is_healthy",
		Help:      "1 if the PostgreSQL cluster has an elected leader, 0 otherwise.",
	}, []string{"namespace", "cluster"})

	// ReplicasInSync is the number of streaming replicas that report zero lag.
	ReplicasInSync = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "pgc",
		Name:      "replicas_in_sync",
		Help:      "Number of replicas that are running with zero replication lag.",
	}, []string{"namespace", "cluster"})

	// FailoverSeconds is the observed failover window: the time between the
	// last observation of the previous leader and the first observation of the
	// new one. Resolution is bounded by the operator reconcile interval.
	FailoverSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "pgc",
		Name:      "failover_duration_seconds",
		Help:      "Observed leader failover duration in seconds.",
	}, []string{"namespace", "cluster"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(LeaderHealthy, ReplicasInSync, FailoverSeconds)
}

// DeleteCluster removes all metric series for a deleted cluster.
func DeleteCluster(namespace, cluster string) {
	labels := prometheus.Labels{"namespace": namespace, "cluster": cluster}
	LeaderHealthy.Delete(labels)
	ReplicasInSync.Delete(labels)
	FailoverSeconds.Delete(labels)
}
