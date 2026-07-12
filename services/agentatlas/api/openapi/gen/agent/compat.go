package agentapi

// These aliases preserve the public names emitted before the change-decision
// enum introduced a generator-level Reject collision.
const (
	Comment       AnnotateDreamRunJSONBodyAction = AnnotateDreamRunJSONBodyActionComment
	Confirm       AnnotateDreamRunJSONBodyAction = AnnotateDreamRunJSONBodyActionConfirm
	MarkIncorrect AnnotateDreamRunJSONBodyAction = AnnotateDreamRunJSONBodyActionMarkIncorrect
	Reject        AnnotateDreamRunJSONBodyAction = AnnotateDreamRunJSONBodyActionReject
)
