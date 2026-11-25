package service

type ResizeAction int

const (
	// ResizeRecreate means the pod has to be deleted and recreated.
	ResizeRecreate ResizeAction = iota
	// ResizeWaiting means an in-place resize of the pod is in progress.
	ResizeWaiting
	// ResizeDone means the pod was resized in place and moved to the update revision.
	ResizeDone
)

// ResizeClass tells whether a later attempt of the resize can succeed.
type ResizeClass string

const (
	// ResizeUserRevision means that the revisions differ in more than cpu and
	// memory, so only a recreate moves the pod.
	ResizeUserRevision ResizeClass = "UserRevision"
	// ResizeTransient means that the resize can still succeed.
	ResizeTransient ResizeClass = "Transient"
	// ResizePermanent means that the cluster refuses the resize, or that the
	// kubelet did not apply it in time.
	ResizePermanent ResizeClass = "Permanent"
)

// AutoscalerLimits are the memory limits of the redis container with
// autoscaling. The zero value means that no step of the autoscaler is
// possible.
type AutoscalerLimits struct {
	// Effective is the limit of the pods, or 0. Spec is the limit of
	// redis.resources, the limit before the step when the spec limit is at
	// the target of the step, or 0 without a known state. Only a revision to
	// an Effective above Spec can be a step.
	Spec, Effective int64
	// Step is the limit of the last autoscaler step, or 0. A spec limit at or
	// above it and a memoryMax below it end it. A pod with this limit keeps
	// it, also when it only requests it since a revert.
	Step int64
	// StepMax is memoryMax when the state of the last step is not known, or
	// 0. Then a pod with a larger limit up to StepMax keeps it, as for Step.
	StepMax int64
}

// ResizeResult is the outcome of ResizePodInPlace.
type ResizeResult struct {
	Action ResizeAction
	// Class is empty for ResizeDone.
	Class ResizeClass
	// Message is the progress of a resize, the reason for a recreate, or
	// empty.
	Message string
	// AutoscalerRevision reports that the update revision is a step of the
	// memory autoscaler from the pod's revision, that the pod requests the
	// Step limit, or that a revision cannot be read with autoscaler limits.
	// The rollout never recreates such a pod.
	AutoscalerRevision bool
}
