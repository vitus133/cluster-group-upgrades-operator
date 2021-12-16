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

	ChildPolicyLabel       = "policy.open-cluster-management.io/root-policy"
	KubeconfigSecretSuffix = "admin-kubeconfig"

	// precaching job constants and states
	PrecacheJobNamespace       = "pre-cache"
	PrecacheJobName            = "pre-cache"
	PrecacheServiceAccountName = "pre-cache-agent"
	JobNotFoundString          = "jobs.batch \"pre-cache\" not found"

	PrecacheNotStarted         = "NotStarted"
	PrecacheActive             = "Active"
	PrecacheSucceeded          = "Succeeded"
	PrecachePartiallyDone      = "PartiallyDone"
	PrecacheUnrecoverableError = "UnrecoverableError"
	PrecacheUnforeseenStatus   = "UnforeseenStatus"
)
