package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	utils "github.com/openshift-kni/cluster-group-upgrades-operator/controllers/utils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// getManagedClusterCredentials gets kubeconfig of the managed cluster by name.
// returns: []byte - the cluster kubeconfig (base64 encoded bytearray)
//			error
func (r *ClusterGroupUpgradeReconciler) getManagedClusterCredentials(
	ctx context.Context,
	cluster string) ([]byte, error) {

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf(
		"%s-%s", cluster, utils.KubeconfigSecretSuffix),
		Namespace: cluster}, secret)
	if err != nil {
		return []byte{}, err
	}
	return secret.Data["kubeconfig"], nil
}

// getSpokeClientset: Connects to the spoke cluster.
// returns: *kubernetes.Clientset - API clientset
//			error
func (r *ClusterGroupUpgradeReconciler) getSpokeClientset(
	kubeconfig []byte) (*kubernetes.Clientset, error) {

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		r.Log.Error(err, "failed to create K8s config")
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		r.Log.Error(err, "failed to create K8s clientset")
	}
	return clientset, err
}

// getPrecacheJobState: Gets the pre-caching state from the spoke.
// returns: string - job state, one of "NotStarted", "Active", "Succeeded",
//                   "PartiallyDone", "UnrecoverableError", "UnforeseenStatus"
//			error
func (r *ClusterGroupUpgradeReconciler) getPrecacheJobState(
	ctx context.Context, clientset *kubernetes.Clientset) (
	string, error) {

	jobs := clientset.BatchV1().Jobs(utils.PrecacheJobNamespace)
	preCacheJob, err := jobs.Get(ctx, utils.PrecacheJobName, metav1.GetOptions{})
	if err != nil {
		if err.Error() == utils.JobNotFoundString {
			return utils.PrecacheNotStarted, nil
		}
		r.Log.Error(err, "get precache job failed")
		return "", err
	}
	if preCacheJob.Status.Active > 0 {
		return utils.PrecacheActive, nil
	}
	if preCacheJob.Status.Succeeded > 0 {
		return utils.PrecacheSucceeded, nil
	}
	for _, condition := range preCacheJob.Status.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			r.Log.Info("getPrecacheJobState", "condition",
				condition.String())
			if condition.Reason == "DeadlineExceeded" {
				return utils.PrecachePartiallyDone, nil
			} else if condition.Reason == "BackoffLimitExceeded" {
				return utils.PrecacheUnrecoverableError, errors.NewInternalError(
					fmt.Errorf(condition.String()))
			}
			break
		}
	}
	jobStatus, err := json.Marshal(preCacheJob.Status)
	if err != nil {
		return "", err
	}
	return utils.PrecacheUnforeseenStatus, errors.NewInternalError(fmt.Errorf(
		string(jobStatus)))
}

// makeContainerMounts: fills the precaching container mounts structure.
// returns: *[]corev1.VolumeMount - volume mount list pointer
func (r *ClusterGroupUpgradeReconciler) makeContainerMounts() *[]corev1.VolumeMount {
	var mounts []corev1.VolumeMount = []corev1.VolumeMount{
		{
			Name:      "cache",
			MountPath: "/cache",
		}, {
			Name:      "varlibcontainers",
			MountPath: "/var/lib/containers",
		}, {
			Name:      "pull",
			MountPath: "/var/lib/kubelet/config.json",
			ReadOnly:  true,
		}, {
			Name:      "config-volume",
			MountPath: "/etc/config",
			ReadOnly:  true,
		}, {
			Name:      "registries",
			MountPath: "/etc/containers/registries.conf",
			ReadOnly:  true,
		}, {
			Name:      "policy",
			MountPath: "/etc/containers/policy.json",
			ReadOnly:  true,
		}, {
			Name:      "etcdocker",
			MountPath: "/etc/docker",
			ReadOnly:  true,
		}, {
			Name:      "usr",
			MountPath: "/usr",
			ReadOnly:  true,
		},
	}
	return &mounts
}

// makePodVolumes: fills the precaching pod volumes structure.
// returns: *[]corev1.Volume - volume list pointer
func (r *ClusterGroupUpgradeReconciler) makePodVolumes() *[]corev1.Volume {
	dirType := corev1.HostPathDirectory
	fileType := corev1.HostPathFile
	var volumes []corev1.Volume = []corev1.Volume{
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}, {
			Name: "config-volume",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "pre-cache-spec",
					},
				},
			},
		}, {
			Name: "varlibcontainers",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/containers",
					Type: &dirType,
				},
			},
		}, {
			Name: "registries",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/containers/registries.conf",
					Type: &fileType,
				},
			},
		}, {
			Name: "policy",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/containers/policy.json",
					Type: &fileType,
				},
			},
		}, {
			Name: "etcdocker",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/docker",
					Type: &dirType,
				},
			},
		}, {
			Name: "usr",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/usr",
					Type: &dirType,
				},
			},
		}, {
			Name: "pull",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/config.json",
					Type: &fileType,
				},
			},
		},
	}
	return &volumes
}

// makeContainerEnv: fills the precaching container environment variables.
// returns: *[]corev1.EnvVar - EnvVar list pointer
func (r *ClusterGroupUpgradeReconciler) makeContainerEnv(deadline int64) *[]corev1.EnvVar {
	var envs []corev1.EnvVar = []corev1.EnvVar{
		{
			Name:  "pull_timeout",
			Value: strconv.FormatInt(deadline, 10),
		},
		{
			Name:  "config_volume_path",
			Value: "/etc/config",
		},
	}
	return &envs
}

// createPrecacheJob: Creates a new pre-cache job on the spoke.
// returns: error
func (r *ClusterGroupUpgradeReconciler) createPrecacheJob(ctx context.Context, clientset *kubernetes.Clientset, image string, deadline int64) error {
	jobs := clientset.BatchV1().Jobs(utils.PrecacheJobNamespace)
	cont := fmt.Sprintf("%s-container", utils.PrecacheJobName)
	volumes := r.makePodVolumes()
	mounts := r.makeContainerMounts()
	envs := r.makeContainerEnv(deadline)
	jobSpec := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.PrecacheJobName,
			Namespace: utils.PrecacheJobNamespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          new(int32),
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    cont,
							Image:   image,
							Command: []string{"/bin/bash", "-c"},
							//Args:    []string{"/opt/precache/precache.sh"},
							Args: []string{"sleep inf"},
							Env:  *envs,
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
								RunAsUser:  new(int64),
							},
							VolumeMounts: *mounts,
						},
					},
					ServiceAccountName: utils.PrecacheServiceAccountName,
					RestartPolicy:      corev1.RestartPolicyNever,
					Volumes:            *volumes,
				},
			},
		},
	}

	_, err := jobs.Create(ctx, jobSpec, metav1.CreateOptions{})

	if err != nil {
		r.Log.Error(err, "createPrecacheJob")
		return err
	}
	r.Log.Info("createPrecacheJob", "createPrecacheJob", "success")
	return nil
}
