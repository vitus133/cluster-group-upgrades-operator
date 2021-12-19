package utils

const (
	RemediationActionEnforce = "enforce"
	RemediationActionInform  = "inform"

	StatusNonCompliant          = "NonCompliant"
	StatusCompliant             = "Compliant"
	ClusterNotMatchedWithPolicy = "NotMatchedWithPolicy"
	StatusUnknown               = "StatusUnknown"

	NoPolicyIndex        = -1
	AllPoliciesValidated = -2

	ChildPolicyLabel        = "policy.open-cluster-management.io/root-policy"
	KubeconfigSecretSuffix  = "admin-kubeconfig"
	OperatorConfigOverrides = "cluster-group-upgrade-overrides"

	// precaching job constants and states
	PrecacheJobNamespace       = "pre-cache"
	PrecacheJobName            = "pre-cache"
	PrecacheServiceAccountName = "pre-cache-agent"
	PrecacheSpecCmName         = "pre-cache-spec"
	JobNotFoundString          = "jobs.batch \"pre-cache\" not found"

	PrecacheNotStarted         = "NotStarted"
	PrecacheStarting           = "Starting"
	PrecacheFailedToStart      = "FailiedToStart"
	PrecacheActive             = "Active"
	PrecacheSucceeded          = "Succeeded"
	PrecachePartiallyDone      = "PartiallyDone"
	PrecacheUnrecoverableError = "UnrecoverableError"
	PrecacheUnforeseenStatus   = "UnforeseenStatus"
)
