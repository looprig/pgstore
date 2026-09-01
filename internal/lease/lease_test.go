package lease

import (
	"testing"
	"time"
)

func TestConservativeRemainingSubtractsTheWholeProofDuration(t *testing.T) {
	databaseNow := time.Now()
	proofStarted := time.Now().Add(-100 * time.Millisecond)
	expiresAt := databaseNow.Add(50 * time.Millisecond)
	if remaining := conservativeRemaining(proofStarted, expiresAt, databaseNow); remaining > 0 {
		t.Fatalf("conservative remaining = %s, want nonpositive after proof latency crossed expiry", remaining)
	}
}
