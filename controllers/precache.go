package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"strings"

	ranv1alpha1 "github.com/openshift-kni/cluster-group-upgrades-operator/api/v1alpha1"
	utils "github.com/openshift-kni/cluster-group-upgrades-operator/controllers/utils"
	v1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type precachingSpec struct {
	PlatformImage                string
	OperatorsIndexes             []string
	OperatorsPackagesAndChannels []string
}

// reconcilePrecaching: main precaching loop function
// returns: 			error
func (r *ClusterGroupUpgradeReconciler) reconcilePrecaching(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) error {

	if clusterGroupUpgrade.Spec.PreCaching {
		// Pre-caching is required
		doneCondition := meta.FindStatusCondition(
			clusterGroupUpgrade.Status.Conditions, "PrecachingDone")
		r.Log.Info("[reconcilePrecaching]",
			"FindStatusCondition  PrecachingDone", doneCondition)
		if doneCondition != nil && doneCondition.Status == metav1.ConditionTrue {
			// Precaching is done
			return nil
		}
		// Precaching is required and not marked as done
		return r.updatePrecachingStatus(ctx, clusterGroupUpgrade)
	}
	// No precaching required
	return nil
}

// checkPrecacheDependencies: check all precache job dependencies
//		have been deployed
// returns: 	available (bool) - deployed andavailable (true),
//		otherwise false
//				error
func (r *ClusterGroupUpgradeReconciler) checkPrecacheDependencies(
	ctx context.Context, cluster string) (bool, error) {

	for _, item := range precacheDependenciesViewTemplates {
		available, err := r.isViewChildResourceAvailable(
			ctx, item.resourceName, cluster)
		if err != nil {
			return false, err
		}
		if !available {
			return false, nil
		}
	}
	return true, nil
}

// updatePrecachingStatus: iterates over clusters and checks precaching status
// returns: error
func (r *ClusterGroupUpgradeReconciler) updatePrecachingStatus(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) error {

	crFirstRun := len(clusterGroupUpgrade.Status.PrecacheStatus) == 0
	clusters, err := r.getAllClustersForUpgrade(ctx, clusterGroupUpgrade)
	if err != nil {
		return fmt.Errorf("cannot obtain the CGU cluster list: %s", err)
	}

	meta.SetStatusCondition(
		&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "PrecachingRequired",
			Message: "Precaching is not completed (required)"})

	meta.SetStatusCondition(
		&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
			Type:    "PrecachingDone",
			Status:  metav1.ConditionFalse,
			Reason:  "PrecachingNotDone",
			Message: "Precaching is required and not done"})

	clusterState := make(map[string]string)
	for _, cluster := range clusters {
		if crFirstRun {
			// Cleanup all existing view objects that might have been left behind
			// in case of a crash etc.
			for _, item := range allPossibleClusterViewsForDelete {
				err := r.deleteManagedClusterViewResource(ctx, item.resourceName, cluster)
				if err != nil {
					if !errors.IsNotFound(err) {
						return err
					}
				}
			}
		}

		if clusterGroupUpgrade.Status.PrecacheStatus[cluster] == utils.PrecacheStarting {
			ready, err := r.checkPrecacheDependencies(ctx, cluster)
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
			err = r.deployPrecachingWorkload(ctx, clusterGroupUpgrade, cluster)
			if err != nil {
				return err
			}
		} else if clusterGroupUpgrade.Status.PrecacheStatus[cluster] == utils.PrecacheRestarting {
			// Wait for pre-cache namespace deletion on the spoke
			available, err := r.isViewChildResourceAvailable(
				ctx, "view-precache-namespace", cluster)
			if err != nil {
				return err
			}
			if !available {
				err = r.deleteManagedClusterViewResource(ctx, "view-precache-namespace", cluster)
				if err != nil {
					return err
				}
			} else {
				continue
			}
		}

		jobStatus, err := r.getPrecacheJobState(ctx, cluster)
		if err != nil {
			return err
		}

		clusterState[cluster] = jobStatus
		switch jobStatus {
		case utils.PrecachePartiallyDone:
			if crFirstRun {
				// This condition means that there is a pre-cache job created
				// on the previous mtce window, but there was not enough time
				// to complete it. The CGU was deleted and re-created.
				// In this case we delete the precaching and create it again
				// once deleted
				// 1. This deletes the precaching namespace on the spoke and
				//    job-view resource on the hub:
				err = r.undeployPrecachingWorkload(ctx, clusterGroupUpgrade, cluster)
				if err != nil {
					return err
				}
				// 2. deploy the spoke pre-cache view resource to watch for
				//    the the namespace delete completion
				spec := templateData{
					Cluster: cluster,
				}
				err := r.createResourcesFromTemplates(ctx, &spec, precacheNSViewTemplates)
				if err != nil {
					return err
				}
				jobStatus = utils.PrecacheRestarting
			}
		case utils.PrecacheNotStarted:
			// PrecacheNotStarted is reported when there is no job watcher found or
			// when there is no job found on the spoke. In this case deploy the
			// dependencies and check for their readiness on the next iteration
			err = r.deployPrecachingDependencies(ctx, clusterGroupUpgrade, cluster)
			if err != nil {
				return err
			}
			clusterState[cluster] = utils.PrecacheStarting
		}
	}

	clusterGroupUpgrade.Status.PrecacheStatus = make(map[string]string)
	clusterGroupUpgrade.Status.PrecacheStatus = clusterState

	// Handle utils.PrecacheFailedToStart alleviation
	if func() bool {
		for _, state := range clusterState {
			if state == utils.PrecacheFailedToStart {
				return false
			}
		}
		return true
	}() {
		if meta.IsStatusConditionPresentAndEqual(
			clusterGroupUpgrade.Status.Conditions, "PrecachingCanStart", metav1.ConditionFalse) {
			meta.RemoveStatusCondition(&clusterGroupUpgrade.Status.Conditions, "PrecachingCanStart")
		}
	}

	// Handle completion
	if func() bool {
		for _, state := range clusterState {
			if state != utils.PrecacheSucceeded {
				return false
			}
		}
		return true
	}() {
		meta.SetStatusCondition(
			&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionFalse,
				Reason:  "UpgradeNotStarted",
				Message: "Precaching is completed"})
		meta.SetStatusCondition(
			&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
				Type:    "PrecachingDone",
				Status:  metav1.ConditionTrue,
				Reason:  "PrecachingCompleted",
				Message: "Precaching is completed"})
		meta.RemoveStatusCondition(&clusterGroupUpgrade.Status.Conditions, "PrecacheSpecValid")
	}
	return nil
}

// getPrecachingSpecFromPolicies: extract the precaching spec from policies
//		in the CGU namespace. There are three object types to look at:
//      - ClusterVersion: release image must be specified to be pre-cached
//      - Subscription: provides the list of operator packages and channels
//      - CatalogSource: must be explicitly configured to be precached.
//        All the clusters in the CGU must have same catalog source(s)
// returns: precachingSpec, error

func (r *ClusterGroupUpgradeReconciler) getPrecachingSpecFromPolicies(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) (precachingSpec, error) {

	var spec precachingSpec
	policiesList, err := r.getPoliciesForNamespace(ctx, clusterGroupUpgrade.GetNamespace())
	if err != nil {
		return spec, err
	}

	for _, policy := range policiesList.Items {
		// Filter by explicit list in CGU

		if !func(a string, list []string) bool {
			for _, b := range list {
				if b == a {
					return true
				}
			}
			return false
		}(policy.GetName(), clusterGroupUpgrade.Spec.ManagedPolicies) {
			r.Log.Info("[getPrecachingSpecFromPolicies]", "Skip policy",
				policy.GetName(), "reason", "Not in CGU")
			continue
		}

		objects, err := r.stripPolicy(policy.Object)
		if err != nil {
			return spec, err
		}
		for _, object := range objects {
			kind := object["kind"]
			switch kind {
			case "ClusterVersion":
				image := object["spec"].(map[string]interface {
				})["desiredUpdate"].(map[string]interface{})["image"]
				if len(spec.PlatformImage) > 0 {
					msg := fmt.Sprintf("Platform image must be set once, but %s and %s were given",
						spec.PlatformImage, image)
					meta.SetStatusCondition(&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
						Type:    "PrecacheSpecValid",
						Status:  metav1.ConditionFalse,
						Reason:  "PlatformImageConflict",
						Message: msg})
					return *new(precachingSpec), nil
				}
				spec.PlatformImage = fmt.Sprintf("%s", image)
				r.Log.Info("[getPrecachingSpecFromPolicies]", "ClusterVersion image", image)
			case "Subscription":
				packChan := fmt.Sprintf("%s:%s", object["spec"].(map[string]interface{})["name"],
					object["spec"].(map[string]interface{})["channel"])
				spec.OperatorsPackagesAndChannels = append(spec.OperatorsPackagesAndChannels, packChan)
				r.Log.Info("[getPrecachingSpecFromPolicies]", "Operator package:channel", packChan)
				continue
			case "CatalogSource":
				index := fmt.Sprintf("%s", object["spec"].(map[string]interface{})["image"])
				spec.OperatorsIndexes = append(spec.OperatorsIndexes, index)
				r.Log.Info("[getPrecachingSpecFromPolicies]", "CatalogSource", index)
				continue
			default:
				continue
			}
		}
	}
	return spec, nil
}

// stripPolicy strips policy information and returns the underlying objects
// returns: []interface{} - list of the underlying objects in the policy
//			error
func (r *ClusterGroupUpgradeReconciler) stripPolicy(
	policyObject map[string]interface{}) ([]map[string]interface{}, error) {

	var objects []map[string]interface{}

	policyTemplates, exists, err := unstructured.NestedFieldCopy(
		policyObject, "spec", "policy-templates")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("[stripPolicy] spec -> policy-templates not found")
	}

	for _, policyTemplate := range policyTemplates.([]interface{}) {
		objTemplates := policyTemplate.(map[string]interface {
		})["objectDefinition"].(map[string]interface {
		})["spec"].(map[string]interface{})["object-templates"]
		if policyTemplate == nil {
			return nil, fmt.Errorf("[stripPolicy] can't find object-templates in policyTemplate")
		}
		for _, objTemplate := range objTemplates.([]interface{}) {
			spec := objTemplate.(map[string]interface{})["objectDefinition"]
			if spec == nil {
				return nil, fmt.Errorf("[stripPolicy] can't find any objectDefinition")
			}
			objects = append(objects, spec.(map[string]interface{}))
		}
	}
	return objects, nil
}

// deployPrecachingWorkload deploys precaching workload on the spoke
//          using a set of templated manifests
// returns: error
func (r *ClusterGroupUpgradeReconciler) deployPrecachingWorkload(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade,
	cluster string) error {

	spec, err := r.getPrecacheJobTemplateData(ctx, clusterGroupUpgrade, cluster)
	if err != nil {
		return err
	}
	r.Log.Info("[deployPrecachingWorkload]", "getPrecacheJobTemplateData",
		cluster, "status", "success")

	err = r.createResourcesFromTemplates(ctx, spec, precacheCreateTemplates)
	if err != nil {
		return err
	}
	err = r.deletePrecacheDependenciesView(ctx, cluster)
	if err != nil {
		return err
	}
	return nil
}

// deletePrecacheDependenciesView: deletes views of precaching dependencies
// returns: 	error
func (r *ClusterGroupUpgradeReconciler) deletePrecacheDependenciesView(
	ctx context.Context, cluster string) error {
	for _, item := range precacheDependenciesViewTemplates {
		err := r.deleteManagedClusterViewResource(
			ctx, item.resourceName, cluster)
		if err != nil {
			return err
		}
	}
	return nil
}

// deployPrecachingDependencies deploys precaching workload dependencies on the spoke
//          using a set of templated manifests. Dependencies must be fully reconciled
//          on the spoke before precaching job creation is attempted (if not, the job
//			will fail).
// returns: error
func (r *ClusterGroupUpgradeReconciler) deployPrecachingDependencies(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade,
	cluster string) error {

	spec, err := r.getPrecacheSoftwareSpec(ctx, clusterGroupUpgrade, cluster)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%v", spec)
	r.Log.Info("[deployPrecachingDependencies]", "getPrecacheSoftwareSpec",
		cluster, "status", "success", "content", msg)

	ok, msg := r.checkPreCacheSpecConsistency(*spec)
	if !ok {
		meta.SetStatusCondition(&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
			Type:    "PrecacheSpecValid",
			Status:  metav1.ConditionFalse,
			Reason:  "PrecacheSpecIsIncomplete",
			Message: msg})
		return nil
	}
	meta.SetStatusCondition(&clusterGroupUpgrade.Status.Conditions, metav1.Condition{
		Type:    "PrecacheSpecValid",
		Status:  metav1.ConditionTrue,
		Reason:  "PrecacheSpecIsWellFormed",
		Message: msg})

	err = r.createResourcesFromTemplates(ctx, spec, precacheDependenciesCreateTemplates)
	if err != nil {
		return err
	}
	err = r.createResourcesFromTemplates(ctx, spec, precacheDependenciesViewTemplates)
	if err != nil {
		return err
	}

	return nil
}

// undeployPrecachingWorkload cleans up everything in the precaching namespace from the spoke
// returns: error
func (r *ClusterGroupUpgradeReconciler) undeployPrecachingWorkload(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade,
	cluster string) error {

	spec := templateData{
		Cluster: cluster,
	}
	err := r.createResourcesFromTemplates(ctx, &spec, precacheDeleteTemplates)
	if err != nil {
		return err
	}
	err = r.deleteManagedClusterViewResource(ctx, "view-precache-job", cluster)
	if err != nil {
		return err
	}
	return nil
}

// getMyCsv gets CGU clusterserviceversion.
// returns: []byte - the cluster kubeconfig (base64 encoded bytearray)
//			error
func (r *ClusterGroupUpgradeReconciler) getMyCsv(
	ctx context.Context) (map[string]interface{}, error) {

	config := ctrl.GetConfigOrDie()
	dynamic := dynamic.NewForConfigOrDie(config)
	resourceID := schema.GroupVersionResource{
		Group:    "operators.coreos.com",
		Version:  "v1alpha1",
		Resource: "clusterserviceversions",
	}
	list, err := dynamic.Resource(resourceID).List(ctx, metav1.ListOptions{})

	if err != nil {
		return nil, err
	}

	for _, item := range list.Items {
		name := fmt.Sprintf("%s", item.Object["metadata"].(map[string]interface{})["name"])
		if strings.Contains(name, utils.CsvNamePrefix) {
			r.Log.Info("[getMyCsv]", "item", name)
			return item.Object, nil
		}
	}

	return nil, fmt.Errorf("CSV %s not found", utils.CsvNamePrefix)
}

// getPrecacheJobState: Gets the pre-caching state from the ManagedClusterView
//          monitoring the spoke.
// returns: string - job state, one of "NotStarted", "Active", "Succeeded",
//                   "PartiallyDone", "UnrecoverableError", "UnforeseenStatus"
//			error
func (r *ClusterGroupUpgradeReconciler) getPrecacheJobState(
	ctx context.Context, cluster string) (
	string, error) {

	jobView := &unstructured.Unstructured{}
	jobView.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "view.open-cluster-management.io",
		Kind:    "ManagedClusterView",
		Version: "v1beta1",
	})
	err := r.Client.Get(ctx, client.ObjectKey{
		Name:      "view-precache-job",
		Namespace: cluster,
	}, jobView)
	if err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("[getPrecacheJobState]", "return", utils.PrecacheNotStarted)
			return utils.PrecacheNotStarted, nil
		}
		r.Log.Error(err, "[getPrecacheJobState] get ManagedClusterView failed")
		return utils.PrecacheUnforeseenStatus, err
	}

	viewConditions, exists, err := unstructured.NestedSlice(
		jobView.Object, "status", "conditions")
	if !exists {
		return utils.PrecacheUnforeseenStatus, fmt.Errorf(
			"[getPrecacheJobState] no ManagedClusterView conditions found")
	}
	if err != nil {
		return utils.PrecacheUnforeseenStatus, err
	}
	var status string
	var message string
	for _, condition := range viewConditions {
		if condition.(map[string]interface{})["type"] == "Processing" {
			status = condition.(map[string]interface{})["status"].(string)
			message = condition.(map[string]interface{})["message"].(string)
			break
		}
	}

	// Since the job and its view are always created and deleted together,
	// this would be a transient state where the view has been created, but the job
	// is not yet present on the spoke.
	if status == "False" {
		r.Log.Info("[getPrecacheJobState]", "viewStatus", message)
		return utils.PrecacheNotStarted, nil
	}

	usJobStatus, exists, err := unstructured.NestedFieldCopy(
		jobView.Object, "status", "result", "status")
	if !exists {
		return utils.PrecacheUnforeseenStatus, fmt.Errorf(
			"[getPrecacheJobState] no job status found in ManagedClusterView")
	}
	if err != nil {
		return utils.PrecacheUnforeseenStatus, err
	}

	btJobStatus, err := json.Marshal(usJobStatus)
	if err != nil {
		return utils.PrecacheUnforeseenStatus, err
	}

	var jobStatus v1.JobStatus
	err = json.Unmarshal(btJobStatus, &jobStatus)
	if err != nil {
		return utils.PrecacheUnforeseenStatus, err
	}
	r.Log.Info("[getPrecacheJobState]", "cluster", cluster, "JobStatus", jobStatus)
	if jobStatus.Active > 0 {
		return utils.PrecacheActive, nil
	}
	if jobStatus.Succeeded > 0 {
		return utils.PrecacheSucceeded, nil
	}

	for _, condition := range jobStatus.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			r.Log.Info("getPrecacheJobState", "condition",
				condition.String())
			if condition.Reason == "DeadlineExceeded" {
				r.Log.Info("getPrecacheJobState", "DeadlineExceeded",
					"Partially done")
				return utils.PrecachePartiallyDone, nil
			} else if condition.Reason == "BackoffLimitExceeded" {
				return utils.PrecacheUnrecoverableError, nil
			}
			break
		}
	}

	return utils.PrecacheUnforeseenStatus, fmt.Errorf(string(btJobStatus))
}

// getPrecacheimagePullSpec: Get the precaching workload image pull spec.
// returns: image - pull spec string
//          error
func (r *ClusterGroupUpgradeReconciler) getPrecacheimagePullSpec(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) (
	string, error) {

	overrides, err := r.getOperatorConfigOverrides(ctx, clusterGroupUpgrade)
	if err != nil {
		r.Log.Error(err, "getOperatorConfigOverrides failed ")
		return "", err
	}
	image := overrides["precache.image"]
	if image == "" {
		csv, err := r.getMyCsv(ctx)
		if err != nil {
			return "", err
		}
		spec := csv["spec"]

		imagesList := spec.(map[string]interface{})["relatedImages"]
		for _, item := range imagesList.([]interface{}) {
			if item.(map[string]interface{})["name"] == "pre-caching-workload" {
				r.Log.Info("[getPrecacheimagePullSpec]", "workload image",
					item.(map[string]interface{})["image"].(string))
				return item.(map[string]interface{})["image"].(string), nil
			}
		}
		return "", fmt.Errorf(
			"can't find pre-caching image pull spec in TALO CSV or overrides")
	}
	return image, nil
}

// getPrecacheJobTemplateData: initializes template data for the job creation
// returns: 	*templateData
//				error
func (r *ClusterGroupUpgradeReconciler) getPrecacheJobTemplateData(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade, clusterName string) (
	*templateData, error) {

	rv := new(templateData)

	rv.Cluster = clusterName
	rv.PrecachingJobTimeout = uint64(
		clusterGroupUpgrade.Spec.RemediationStrategy.Timeout) * 60
	image, err := r.getPrecacheimagePullSpec(ctx, clusterGroupUpgrade)
	if err != nil {
		return rv, err
	}
	rv.PrecachingWorkloadImage = image
	return rv, nil
}

// getPrecacheSoftwareSpec: Get precaching payload spec for a cluster. It consists of
//    	several parts that together compose the precaching workload API:
//			1. platform.image (e.g. "quay.io/openshift-release-dev/ocp-release:<tag>").
//          2. operators.indexes - a list of pull specs for OLM index images
//          3. operators.packagesAndChannels - Operator packages and channels
// returns: templateData (softwareSpec)
//          error
func (r *ClusterGroupUpgradeReconciler) getPrecacheSoftwareSpec(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade, clusterName string) (
	*templateData, error) {

	rv := new(templateData)
	rv.Cluster = clusterName

	spec, err := r.getPrecachingSpecFromPolicies(ctx, clusterGroupUpgrade)
	if err != nil {
		return rv, err
	}
	r.Log.Info("[getPrecacheSoftwareSpec]", "PrecacheSoftwareSpec:", spec)

	overrides, err := r.getOperatorConfigOverrides(ctx, clusterGroupUpgrade)
	if err != nil {
		return rv, err
	}

	platformImage := overrides["platform.image"]
	operatorsIndexes := strings.Split(overrides["operators.indexes"], "\n")
	operatorsPackagesAndChannels := strings.Split(overrides["operators.packagesAndChannels"], "\n")
	if platformImage == "" {
		platformImage = spec.PlatformImage
	}
	rv.PlatformImage = platformImage

	if overrides["operators.indexes"] == "" {
		operatorsIndexes = spec.OperatorsIndexes
	}
	rv.Operators.Indexes = operatorsIndexes

	if overrides["operators.packagesAndChannels"] == "" {
		operatorsPackagesAndChannels = spec.OperatorsPackagesAndChannels
	}
	rv.Operators.PackagesAndChannels = operatorsPackagesAndChannels

	if err != nil {
		return rv, err
	}
	return rv, err
}

// checkPreCacheSpecConsistency
// returns: consistent (bool), message (string)
func (r *ClusterGroupUpgradeReconciler) checkPreCacheSpecConsistency(
	spec templateData) (consistent bool, message string) {

	var operatorsRequested, platformRequested bool = true, true
	if len(spec.Operators.Indexes) == 0 {
		operatorsRequested = false
	}
	if spec.PlatformImage == "" {
		platformRequested = false
	}
	if operatorsRequested && len(spec.Operators.PackagesAndChannels) == 0 {
		return false, "inconsistent precaching configuration: olm index provided, but no packages"
	}
	if !operatorsRequested && !platformRequested {
		return false, "inconsistent precaching configuration: no software spec provided"
	}
	return true, ""
}

// precachingCleanup: delete all precaching jobs
// returns: 			error
func (r *ClusterGroupUpgradeReconciler) precachingCleanup(
	ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) error {

	if clusterGroupUpgrade.Spec.PreCaching {
		clusters, err := r.getAllClustersForUpgrade(ctx, clusterGroupUpgrade)
		if err != nil {
			return fmt.Errorf("[precachingCleanup]cannot obtain the CGU cluster list: %s", err)
		}

		for _, cluster := range clusters {
			err = r.undeployPrecachingWorkload(ctx, clusterGroupUpgrade, cluster)
			if err != nil {
				return err
			}

		}
	}
	// No precaching required
	return nil
}
