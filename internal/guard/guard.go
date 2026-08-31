// Package guard contains cross-primitive operation policy and typed scaffold
// errors. Keeping it internal lets each adapter enforce identical behavior.
package guard

import (
	"context"
	"strconv"
)

// DeadlineRequiredError reports an operation invoked without a caller-owned
// context deadline.
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

// RequireDeadline rejects nil and unbounded contexts before an operation can
// acquire a connection or observe database state.
func RequireDeadline(ctx context.Context, operation string) error {
	if ctx == nil {
		return &DeadlineRequiredError{Operation: operation}
	}
	if _, ok := ctx.Deadline(); !ok {
		return &DeadlineRequiredError{Operation: operation}
	}
	return nil
}

// NotImplemented returns the typed scaffold result for an operation that has
// passed its deadline guard.
func NotImplemented(operation string) error {
	return &NotImplementedError{Operation: operation}
}
