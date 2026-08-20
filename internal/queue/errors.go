package queue

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoWork indicates no claimable job was available. It is an expected
// condition on an idle system, not a failure — callers must not log it at
// error level.
var ErrNoWork = errors.New("no claimable work")

// ErrLeaseLost indicates this worker no longer owns the job, because the
// lease expired and another worker reclaimed it. The worker must abandon
// all in-flight work immediately and MUST NOT write further state.
var ErrLeaseLost = errors.New("lease lost")

// ErrNotFound indicates the requested job does not exist.
var ErrNotFound = errors.New("job not found")

// ErrNoStepsToRetry indicates a FAILED job has no FAILED step to
// resume from — it failed before any step ran (e.g. a planner error),
// so there is no partial progress RetryJob's "from the first failed
// step" semantics apply to.
var ErrNoStepsToRetry = errors.New("job has no failed step to retry from")

// IllegalTransitionError reports an attempt to move a job between states
// the lifecycle in PRD §13.1 does not permit. Always a caller bug; never
// retried.
type IllegalTransitionError struct {
	JobID    uuid.UUID
	From, To Status
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal transition for job %s: %s -> %s", e.JobID, e.From, e.To)
}
