package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	utils "github.com/openshift-kni/cluster-group-upgrades-operator/controllers/utils"
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
