package controller

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	dbv1 "github.com/l0rdofduck/postgres-reliability-operator/api/v1"
)

const (
	defaultReplicas   = int32(3)
	defaultImage      = "pgc/postgres-patroni:16"
	defaultStorage    = "1Gi"
	patroniPort       = 8008
	postgresPort      = 5432
	postgresUID       = int64(999)
	postgresGID       = int64(999)
	appLabel          = "app.kubernetes.io/name"
	managedByLabel    = "app.kubernetes.io/managed-by"
	roleLabel         = "role"
	operatorName      = "postgres-reliability-operator"
	configVolumeName  = "patroni-config"
	configMountPath   = "/config"
	dataVolumeName    = "pgdata"
	dataMountPath     = "/home/postgres/pgdata"
	secretSuperuser   = "superuser-password"
	secretReplication = "replication-password"
	secretRestapi     = "restapi-password"
)

func effectiveReplicas(pg *dbv1.PostgresCluster) int32 {
	if pg.Spec.Replicas > 0 {
		return pg.Spec.Replicas
	}
	return defaultReplicas
}

func effectiveImage(pg *dbv1.PostgresCluster) string {
	if pg.Spec.Image != "" {
		return pg.Spec.Image
	}
	return defaultImage
}

func effectiveStorage(pg *dbv1.PostgresCluster) string {
	if pg.Spec.StorageSize != "" {
		return pg.Spec.StorageSize
	}
	return defaultStorage
}

func effectivePatroni(pg *dbv1.PostgresCluster) dbv1.PatroniSpec {
	p := pg.Spec.Patroni
	if p.TTL <= 0 {
		p.TTL = 20
	}
	if p.LoopWait <= 0 {
		p.LoopWait = 10
	}
	if p.RetryTimeout <= 0 {
		p.RetryTimeout = 10
	}
	return p
}

func configMapName(pg *dbv1.PostgresCluster) string       { return pg.Name + "-patroni-config" }
func headlessServiceName(pg *dbv1.PostgresCluster) string { return pg.Name }
func primaryServiceName(pg *dbv1.PostgresCluster) string  { return pg.Name + "-primary" }
func replicaServiceName(pg *dbv1.PostgresCluster) string  { return pg.Name + "-replicas" }
func serviceAccountName(pg *dbv1.PostgresCluster) string  { return pg.Name }
func patroniRoleName(pg *dbv1.PostgresCluster) string     { return pg.Name + "-patroni" }

func labels(pg *dbv1.PostgresCluster) map[string]string {
	return map[string]string{
		appLabel:       pg.Name,
		managedByLabel: operatorName,
		"cluster-name": pg.Name,
	}
}

// patroniConfig renders the patroni.yml stored in the cluster ConfigMap.
// Passwords and per-pod values are injected through environment variables.
func patroniConfig(pg *dbv1.PostgresCluster) string {
	p := effectivePatroni(pg)
	cfg := fmt.Sprintf(`scope: %s

kubernetes:
  namespace: %s
  labels:
    app.kubernetes.io/name: %s
    app.kubernetes.io/managed-by: %s
  role_label: role
  use_endpoints: false
  ports:
    - name: postgresql
      port: %d

bootstrap:
  dcs:
    ttl: %d
    loop_wait: %d
    retry_timeout: %d
    maximum_lag_on_failover: 1048576
    synchronous_mode: false
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        hot_standby: "on"
        max_wal_senders: 10
        max_replication_slots: 10
        wal_log_hints: "on"
        shared_buffers: 128MB
  initdb:
    - encoding: UTF8
    - data-checksums

postgresql:
  listen: 0.0.0.0:%d
  data_dir: %s/data
  authentication:
    superuser:
      username: postgres
    replication:
      username: replicator
  pg_hba:
    - local all all trust
    - host all all 0.0.0.0/0 md5
    - host replication replicator 0.0.0.0/0 md5

restapi:
  listen: 0.0.0.0:%d
`,
		pg.Name, pg.Namespace, pg.Name, operatorName, postgresPort,
		p.TTL, p.LoopWait, p.RetryTimeout,
		postgresPort, dataMountPath,
		patroniPort,
	)
	return strings.TrimSpace(cfg) + "\n"
}

func desiredConfigMap(pg *dbv1.PostgresCluster) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(pg),
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Data: map[string]string{"patroni.yml": patroniConfig(pg)},
	}
}

func desiredSecret(pg *dbv1.PostgresCluster, superuser, replication, restapi string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pg.Name,
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			secretSuperuser:   superuser,
			secretReplication: replication,
			secretRestapi:     restapi,
		},
	}
}

func desiredServiceAccount(pg *dbv1.PostgresCluster) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName(pg),
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
	}
}

// desiredPatroniRole grants Patroni the permissions it needs to use Kubernetes
// as its DCS (endpoints) and to patch pod role labels.
func desiredPatroniRole(pg *dbv1.PostgresCluster) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      patroniRoleName(pg),
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"endpoints"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		},
	}
}

func desiredPatroniRoleBinding(pg *dbv1.PostgresCluster) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      patroniRoleName(pg),
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     patroniRoleName(pg),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      serviceAccountName(pg),
			Namespace: pg.Namespace,
		}},
	}
}

func desiredHeadlessService(pg *dbv1.PostgresCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headlessServiceName(pg),
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 labels(pg),
			Ports: []corev1.ServicePort{
				{Name: "postgresql", Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort)},
				{Name: "patroni", Port: patroniPort, TargetPort: intstr.FromInt32(patroniPort)},
			},
		},
	}
}

func desiredRoleService(pg *dbv1.PostgresCluster, name, role string) *corev1.Service {
	selector := labels(pg)
	selector[roleLabel] = role
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{
				{Name: "postgresql", Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort)},
			},
		},
	}
}

func desiredStatefulSet(pg *dbv1.PostgresCluster) *appsv1.StatefulSet {
	podLabels := labels(pg)
	podLabels[roleLabel] = "replica"

	storage := resource.MustParse(effectiveStorage(pg))
	replicas := effectiveReplicas(pg)

	env := []corev1.EnvVar{
		{
			Name: "PATRONI_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
		{Name: "PATRONI_SCOPE", Value: pg.Name},
		{Name: "PATRONI_KUBERNETES_NAMESPACE", Value: pg.Namespace},
		{Name: "PATRONI_RESTAPI_LISTEN", Value: fmt.Sprintf("0.0.0.0:%d", patroniPort)},
		{Name: "PATRONI_RESTAPI_USERNAME", Value: "patroni"},
		{
			Name: "PATRONI_RESTAPI_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pg.Name},
					Key:                  secretRestapi,
				},
			},
		},
		{
			Name: "PATRONI_SUPERUSER_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pg.Name},
					Key:                  secretSuperuser,
				},
			},
		},
		{
			Name: "PATRONI_REPLICATION_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pg.Name},
					Key:                  secretReplication,
				},
			},
		},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
			},
		},
		{Name: "PATRONI_RESTAPI_CONNECT_ADDRESS", Value: fmt.Sprintf("$(POD_IP):%d", patroniPort)},
		{Name: "PATRONI_POSTGRESQL_CONNECT_ADDRESS", Value: fmt.Sprintf("$(POD_IP):%d", postgresPort)},
		{Name: "PATRONI_POSTGRESQL_LISTEN", Value: fmt.Sprintf("0.0.0.0:%d", postgresPort)},
	}

	probe := func(path string, failure int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: path,
					Port: intstr.FromInt32(patroniPort),
				},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
			FailureThreshold:    failure,
		}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pg.Name,
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicas,
			ServiceName:         headlessServiceName(pg),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels(pg)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccountName(pg),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  ptrInt64(postgresUID),
						RunAsGroup: ptrInt64(postgresGID),
						FSGroup:    ptrInt64(postgresGID),
					},
					Containers: []corev1.Container{{
						Name:            "postgres",
						Image:           effectiveImage(pg),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{configMountPath + "/patroni.yml"},
						Env:             env,
						Ports: []corev1.ContainerPort{
							{Name: "postgresql", ContainerPort: postgresPort},
							{Name: "patroni", ContainerPort: patroniPort},
						},
						Resources:      pg.Spec.Resources,
						ReadinessProbe: probe("/readiness", 6),
						LivenessProbe:  probe("/liveness", 12),
						VolumeMounts: []corev1.VolumeMount{
							{Name: configVolumeName, MountPath: configMountPath, ReadOnly: true},
							{Name: dataVolumeName, MountPath: dataMountPath},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							RunAsNonRoot:             boolPtr(true),
						},
					}},
					Volumes: []corev1.Volume{{
						Name: configVolumeName,
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(pg)},
							},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   dataVolumeName,
					Labels: labels(pg),
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
					},
				},
			}},
		},
	}
	return sts
}

func desiredPDB(pg *dbv1.PostgresCluster) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pg.Name,
			Namespace: pg.Namespace,
			Labels:    labels(pg),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: labels(pg)},
		},
	}
}

func ptrInt64(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }
