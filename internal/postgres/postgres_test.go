package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryableClassifiesOnlySerializationAndDeadlockSQLStates(t *testing.T) {
	tests := []struct {
		name, code string
		wrap, want bool
	}{
		{name: "serialization", code: "40001", want: true},
		{name: "wrapped deadlock", code: "40P01", wrap: true, want: true},
		{name: "lock unavailable", code: "55P03"},
		{name: "connection failure", code: "08006"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error = &pgconn.PgError{Code: tt.code}
			if tt.wrap {
				err = errors.Join(errors.New("outer"), err)
			}
			if got := Retryable(err); got != tt.want {
				t.Fatalf("Retryable(SQLSTATE %s) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
	if Retryable(errors.New("serialization failure 40001")) {
		t.Fatal("Retryable string-only error = true, want typed SQLSTATE classification")
	}
}
