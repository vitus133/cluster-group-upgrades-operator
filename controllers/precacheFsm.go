package controllers

import (
	"context"
	"fmt"

	ranv1alpha1 "github.com/openshift-kni/cluster-group-upgrades-operator/api/v1alpha1"
)

// Pre-cache states
const (
	PrecacheStateNotStarted = "NotStarted"
	PrecacheStateStarting   = "Starting"
	PrecacheStateRestarting = "Restarting"
	PrecacheStateActive     = "Active"
	PrecacheStateSucceeded  = "Succeeded"
	PrecacheStateTimeout    = "PrecacheTimeout"
	PrecacheStateError      = "UnrecoverableError"
)

// Pre-cache resources conditions
const (
	NoJobView                       = "NoJobView"
	NoJobFoundOnSpoke               = "NoJobFoundOnSpoke"
	JobViewExists                   = "JobViewExists"
	DependenciesNotPresent          = "DependenciesNotPresent"
	DependenciesPresent             = "DependenciesPresent"
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

	r.setPrecachingRequiredConditions(clusterGroupUpgrade)
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
			//Check no precaching NS present
			present, err := r.checkPrecachePresent(ctx, cluster)
			if err != nil {
				return err
			}
			if present {
				err = r.undeployPrecachingWorkload(ctx, cluster)
				nextState = currentState
				r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", "PrecachePresent",
					"cluster", cluster, "nextState", nextState)
			} else {
				data := templateData{
					Cluster: cluster,
				}
				err = r.createResourcesFromTemplates(ctx, &data, precacheJobView)
				nextState = PrecacheStateStarting
				r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", "PrecacheNotPresent",
					"cluster", cluster, "nextState", nextState)
			}
			if err != nil {
				return err
			}

		// Final states that don't change for the life of the CR
		case PrecacheStateSucceeded, PrecacheStateTimeout, PrecacheStateError:
			nextState = currentState
			r.Log.Info("[precachingFsm]", "cluster", cluster, "final state", currentState)

		case PrecacheStateActive:
			condition, err := r.getPrecacheCondition(ctx, cluster)
			if err != nil {
				return err
			}
			switch condition {
			case PrecacheJobDeadline:
				nextState = PrecacheStateTimeout
			case PrecacheJobSucceeded:
				nextState = PrecacheStateSucceeded
			case PrecacheJobBackoffLimitExceeded:
				nextState = PrecacheStateError
			case PrecacheJobActive:
				nextState = PrecacheStateActive
			default:
				return fmt.Errorf("[precachingFsm] unknown condition %s in %s state", condition, currentState)
			}
		default:
			return fmt.Errorf("[precachingFsm] unknown state %s", currentState)

		}

		clusterStates[cluster] = nextState
		r.Log.Info("[precachingFsm]", "previousState", currentState, "nextState", nextState, "cluster", cluster)

	}
	clusterGroupUpgrade.Status.PrecacheStatus = make(map[string]string)
	clusterGroupUpgrade.Status.PrecacheStatus = clusterStates
	r.handleCguStates(clusterGroupUpgrade)
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
		condition = JobViewExists
	} else {
		data := templateData{
			Cluster: cluster,
		}
		err = r.createResourcesFromTemplates(ctx, &data, precacheJobView)
		nextState = PrecacheStateStarting
	}
	r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", condition,
		"cluster", cluster, "action", "cleanupHubResources", "nextState", nextState)
	if err != nil {
		return nextState, err
	}
	return nextState, nil
}

// handleStarting: Handles conditions in PrecacheStateStart
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
	case DependenciesNotPresent:
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
			"cluster", cluster, "action", "createResourcesFromTemplates", "nextState", PrecacheStateStarting)
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

	r.Log.Info("[precachingFsm]", "currentState", currentState, "condition", condition,
		"cluster", cluster, "action", "cleanupHubResources", "nextState", nextState)

	return nextState, nil
}
