/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	ranv1alpha1 "github.com/openshift-kni/cluster-group-upgrades-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pre-cache states
const (
	PrecacheStateNotStarted  = "NotStarted"
	PrecacheStatePreExisting = "PrecacheStatePreExisting"
	PrecacheStateStarting    = "Starting"
	PrecacheStateRestarting  = "Restarting"
	PrecacheStateActive      = "Active"
	PrecacheStateSucceeded   = "Succeeded"
	PrecacheStateTimeout     = "PrecacheTimeout"
	PrecacheStateError       = "UnrecoverableError"
)

// Pre-cache resources conditions
const (
	NoJobView                       = "NoJobView"
	NoJobFoundOnSpoke               = "NoJobFoundOnSpoke"
	JobViewExists                   = "JobViewExists"
	DependenciesViewNotPresent      = "DependenciesViewNotPresent"
	DependenciesNotPresent          = "DependenciesNotPresent"
	PrecacheJobDeadline             = "PrecacheJobDeadline"
	PrecacheJobSucceeded            = "PrecacheJobSucceeded"
	PrecacheJobActive               = "PrecacheJobActive"
	PrecacheJobBackoffLimitExceeded = "PrecacheJobBackoffLimitExceeded"
	PrecacheUnforeseenCondition     = "UnforeseenCondition"
)

// precachingFsm: Implements the precaching state machine
// returns: error
func (r *ClusterGroupUpgradeReconciler) precachingFsm(ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) error {

	r.setPrecachingRequired(clusterGroupUpgrade)
	clusters, err := r.getAllClustersForUpgrade(ctx, clusterGroupUpgrade)
	if err != nil {
		return fmt.Errorf("cannot obtain the CGU cluster list: %s", err)
	}

	clusterStates := make(map[string]string)
	for _, cluster := range clusters {
		var currentState string
		if len(clusterGroupUpgrade.Status.PrecacheStatus) == 0 {
			currentState = PrecacheStateNotStarted
		} else {
			currentState = clusterGroupUpgrade.Status.PrecacheStatus[cluster]
		}
		var nextState string
		r.Log.Info("[precachingFsm]", "currentState", currentState, "cluster", cluster)
		switch currentState {
		// Initial State
		case PrecacheStateNotStarted:
			nextState, err = r.handleNotStarted(ctx, cluster)
			if err != nil {
				return err
			}
		case PrecacheStateStarting:
			nextState, err = r.handleStarting(ctx, clusterGroupUpgrade, cluster)
			if err != nil {
				return err
			}
		// Restart
		case PrecacheStateRestarting:
			nextState, err = r.handleRestarting(ctx, cluster)
			if err != nil {
				return err
			}

		// Final states that don't change for the life of the CR
		case PrecacheStateSucceeded, PrecacheStateTimeout, PrecacheStateError:
			nextState = currentState
			r.Log.Info("[precachingFsm]", "cluster", cluster, "final state", currentState)

		case PrecacheStateActive:
			nextState, err = r.handleActive(ctx, cluster)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("[precachingFsm] unknown state %s", currentState)

		}

		clusterStates[cluster] = nextState
		r.Log.Info("[precachingFsm]", "previousState", currentState, "nextState", nextState, "cluster", cluster)

	}
	clusterGroupUpgrade.Status.PrecacheStatus = make(map[string]string)
	clusterGroupUpgrade.Status.PrecacheStatus = clusterStates
	r.checkPrecachingCompleted(clusterGroupUpgrade)
	return nil
}

// handleNotStarted: Handles conditions in PrecacheStateNotStarted
// returns: error
func (r *ClusterGroupUpgradeReconciler) handleNotStarted(ctx context.Context,
	cluster string) (string, error) {

	nextState, currentState := PrecacheStateNotStarted, PrecacheStateNotStarted
	var condition string
	// Check for continuation of the previous mtce window
	_, exists, err := r.getView(ctx, "view-precache-job", cluster)
	if err != nil {
		return nextState, err
	}
	if exists {
		// This condition means CR has been deleted and created again
		// We clean up and create view resources again since they are
		// updating periodically and could be outdated
		err = r.cleanupHubResources(ctx, cluster)
		if err != nil {
			return nextState, err
		}
		condition = JobViewExists // for logging

	} else {
		condition = NoJobView
	}

	data := templateData{
		Cluster: cluster,
	}
	err = r.createResourcesFromTemplates(ctx, &data, precacheJobView)
	nextState = PrecacheStateStarting

	r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", condition,
		"cluster", cluster, "nextState", nextState)
	if err != nil {
		return nextState, err
	}
	return nextState, nil
}

// handleStarting: Handles conditions in PrecacheStateStarting
// returns: error
func (r *ClusterGroupUpgradeReconciler) handleStarting(ctx context.Context,
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade,
	cluster string) (string, error) {

	nextState, currentState := PrecacheStateStarting, PrecacheStateStarting
	var condition string

	condition, err := r.getPrecacheCondition(ctx, cluster)
	if err != nil {
		return nextState, err
	}
	switch condition {
	case DependenciesViewNotPresent, DependenciesNotPresent:
		_, err := r.deployPrecachingDependencies(ctx, clusterGroupUpgrade, cluster)
		if err != nil {
			return nextState, err
		}
	case NoJobView:
		data := templateData{
			Cluster: cluster,
		}
		err = r.createResourcesFromTemplates(ctx, &data, precacheNSViewTemplates)
		if err != nil {
			return nextState, err
		}
	case NoJobFoundOnSpoke:
		r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", NoJobFoundOnSpoke,
			"cluster", cluster, "nextState", PrecacheStateStarting)
		err = r.deployPrecachingWorkload(ctx, clusterGroupUpgrade, cluster)
		if err != nil {
			return nextState, err
		}
	case PrecacheJobActive:
		nextState = PrecacheStateActive
	case PrecacheJobSucceeded:
		nextState = PrecacheStateSucceeded
	case PrecacheJobDeadline:
		//Delete all
		err = r.restartPrecaching(ctx, cluster)
		if err != nil {
			return nextState, err
		}
		nextState = PrecacheStateRestarting
	case PrecacheJobBackoffLimitExceeded:
		nextState = PrecacheStateError

	default:
		return nextState, fmt.Errorf(
			"[handleStarting] unknown condition %v in %s state", condition, currentState)
	}

	r.Log.Info("[precachingFsm]", "cluster", cluster, "currentState", currentState,
		"condition", condition, "nextState", nextState)

	return nextState, nil
}

// handleRestarting: Handles conditions in PrecacheStateRestarting
// returns: error
func (r *ClusterGroupUpgradeReconciler) handleRestarting(ctx context.Context,
	cluster string) (string, error) {

	nextState, currentState := PrecacheStateRestarting, PrecacheStateRestarting
	//Check no precaching NS present
	present, err := r.checkPrecacheNsPresent(ctx, cluster)
	if err != nil {
		return nextState, err
	}
	if present {
		// No state change
		err = r.undeployPrecachingWorkload(ctx, cluster)
	} else {
		data := templateData{
			Cluster: cluster,
		}
		err = r.createResourcesFromTemplates(ctx, &data, precacheJobView)
		nextState = PrecacheStateStarting
	}
	r.Log.Info("[precachingFsm]", "currentState", currentState,
		"cluster", cluster, "nextState", nextState)

	if err != nil {
		return nextState, err
	}
	return nextState, nil
}

// handleActive: Handles conditions in PrecacheStateActive
// returns: error
func (r *ClusterGroupUpgradeReconciler) handleActive(ctx context.Context,
	cluster string) (string, error) {

	nextState, currentState := PrecacheStateActive, PrecacheStateActive
	condition, err := r.getPrecacheCondition(ctx, cluster)
	if err != nil {
		return nextState, err
	}
	switch condition {
	case PrecacheJobDeadline:
		nextState = PrecacheStateTimeout
	case PrecacheJobSucceeded:
		err = r.deletePrecacheDependenciesView(ctx, cluster)
		if err != nil {
			return currentState, err
		}
		nextState = PrecacheStateSucceeded
	case PrecacheJobBackoffLimitExceeded:
		nextState = PrecacheStateError
	case PrecacheJobActive:
		nextState = PrecacheStateActive
	default:
		return currentState, fmt.Errorf("[precachingFsm] unknown condition %s in %s state",
			condition, currentState)
	}
	return nextState, nil
}

func (r *ClusterGroupUpgradeReconciler) setPrecachingRequired(
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) {
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
}

// checkPrecachingCompleted: handle alleviation of conditions related to all clusters
func (r *ClusterGroupUpgradeReconciler) checkPrecachingCompleted(
	clusterGroupUpgrade *ranv1alpha1.ClusterGroupUpgrade) {
	// Handle completion
	if func() bool {
		for _, state := range clusterGroupUpgrade.Status.PrecacheStatus {
			if state != PrecacheStateSucceeded {
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
}
