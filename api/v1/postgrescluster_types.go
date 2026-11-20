package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase values reported in PostgresClusterStatus.
const (
	PhaseInitializing = "Initializing"
	PhaseReady        = "Ready"
	PhaseDegraded     = "Degraded"
)

// Condition types reported by the operator.
const (
	ConditionReady          = "Ready"
	ConditionLeaderElected  = "LeaderElected"
	ConditionReplicasInSync = "ReplicasInSync"
)

// PostgresClusterSpec defines the desired state of a PostgreSQL cluster
// managed by Patroni.
type PostgresClusterSpec struct {
	// Replicas is the total number of PostgreSQL instances (leader + replicas).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Image is the PostgreSQL + Patroni image used for every instance.
	// +kubebuilder:default="pgc/postgres-patroni:16"
	Image string `json:"image,omitempty"`

	// StorageSize is the size of the PVC attached to every instance.
	// +kubebuilder:default="1Gi"
	StorageSize string `json:"storageSize,omitempty"`

	// Resources applied to the PostgreSQL container.
	// +kubebuilder:pruning:PreserveUnknownFields
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Patroni holds DCS tuning parameters.
	Patroni PatroniSpec `json:"patroni,omitempty"`
}

// PatroniSpec holds values written into the Patroni DCS configuration.
type PatroniSpec struct {
	// TTL is the DCS lease time-to-live in seconds.
	// +kubebuilder:default=30
	TTL int `json:"ttl,omitempty"`

	// LoopWait is the interval between DCS checks in seconds.
	// +kubebuilder:default=10
	LoopWait int `json:"loopWait,omitempty"`

	// RetryTimeout is the DCS retry timeout in seconds.
	// +kubebuilder:default=10
	RetryTimeout int `json:"retryTimeout,omitempty"`
}

// ReplicaStatus is a snapshot of a single Patroni member.
type ReplicaStatus struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	State    string `json:"state,omitempty"`
	LagBytes int64  `json:"lagBytes"`
}

// PostgresClusterStatus defines the observed state of PostgresCluster.
type PostgresClusterStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Leader             string             `json:"leader,omitempty"`
	Replicas           []ReplicaStatus    `json:"replicas,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Leader",type=string,JSONPath=`.status.leader`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresCluster is the Schema for the postgresclusters API.
type PostgresCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresClusterSpec   `json:"spec,omitempty"`
	Status PostgresClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresClusterList contains a list of PostgresCluster.
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresCluster{}, &PostgresClusterList{})
}
