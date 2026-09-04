package workflow

import "errors"

var (
	// ErrInvalidTransition means the (state, action) pair is not in the graph.
	ErrInvalidTransition = errors.New("invalid transition")
	// ErrWrongActor means the caller is not the role authorized for the action.
	ErrWrongActor = errors.New("wrong actor")
	// ErrStaleVerification means gate evidence no longer matches the inputs.
	ErrStaleVerification = errors.New("stale verification")
	// ErrInvalidPolicy means the policy file was rejected at load time.
	ErrInvalidPolicy = errors.New("invalid policy")
)
