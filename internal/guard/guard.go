// Package guard contains cross-primitive operation policy and typed scaffold
// errors. Keeping it internal lets each adapter enforce identical behavior.
package guard

import (
	"context"
	"strconv"
	"time"
)

// DefaultOperationTimeout bounds an operation whose context carries no
// deadline when the Store was opened without Options.DefaultOperationTimeout.
const DefaultOperationTimeout = 30 * time.Second

// DeadlineRequiredError reports an operation invoked with a nil context. A
// caller that merely omits a deadline no longer receives it: Bound applies the
// Store's default operation timeout instead.
type DeadlineRequiredError struct {
	Operation string
}

func (e *DeadlineRequiredError) Error() string {
	return "pgstore: operation " + strconv.Quote(e.Operation) + " requires a caller context deadline"
}

// NotImplementedError marks the P1.1/P1.2 seam without claiming a successful
// mutation or manufacturing an absent value.
type NotImplementedError struct {
	Operation string
}

func (e *NotImplementedError) Error() string {
	return "pgstore: operation " + strconv.Quote(e.Operation) + " is not implemented"
}

// Bound returns the context an operation runs under. A caller deadline is used
// unchanged, whether shorter or longer than timeout. A context without one
// gets timeout as a child deadline, so the caller's cancellation still reaches
// the operation and pgx still turns expiry into query cancellation. The
// returned cancel must be called when the operation ends. A nil context is
// refused.
func Bound(ctx context.Context, operation string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, &DeadlineRequiredError{Operation: operation}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}, nil
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	return bounded, cancel, nil
}

// NotImplemented returns the typed scaffold result for an operation that has
// passed its deadline guard.
func NotImplemented(operation string) error {
	return &NotImplementedError{Operation: operation}
}
