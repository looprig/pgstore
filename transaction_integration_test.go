//go:build integration

package pgstore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/storage"
)

func TestCanceledBeforeMutationRemainsDefiniteAndDoesNotWrite(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	if err := store.Ledger.Append(ctx, "sessions/pre-canceled", 0, []byte("x")); err != context.Canceled {
		t.Fatalf("Ledger.Append error = %T %v, want context.Canceled", err, err)
	}
	if _, err := store.KV.Put(ctx, "sessions/pre-canceled", 0, []byte("x")); err != context.Canceled {
		t.Fatalf("KV.Put error = %T %v, want context.Canceled", err, err)
	}
}

func TestConcurrentKVCASHasExactlyOneWinnerPerRevision(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const writers = 24
	for expected := uint64(0); expected < 2; expected++ {
		start := make(chan struct{})
		results := make(chan error, writers)
		var wg sync.WaitGroup
		for writer := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.KV.Put(ctx, "sessions/contended", expected, []byte{byte(writer)})
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		winners := 0
		for err := range results {
			if err == nil {
				winners++
				continue
			}
			var conflict *storage.ConflictError
			if !errors.As(err, &conflict) {
				t.Errorf("loser error = %T %v, want *storage.ConflictError", err, err)
			}
		}
		if winners != 1 {
			t.Fatalf("revision %d winners = %d, want exactly 1", expected, winners)
		}
	}
	_, revision, err := store.KV.Get(ctx, "sessions/contended")
	if err != nil || revision != 2 {
		t.Fatalf("Get revision = %d, %v; want 2, nil", revision, err)
	}
}

func TestKVKeysPrefixIsDataNotSQL(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := store.KV.Put(ctx, "sessions/safe", 0, []byte("value")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	keys, err := store.KV.Keys(ctx, `sessions/'; DROP TABLE anything;--`)
	if err != nil {
		t.Fatalf("Keys with SQL-shaped prefix: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("Keys = %v, want empty", keys)
	}
	if _, _, err := store.KV.Get(ctx, "sessions/safe"); err != nil {
		t.Fatalf("Get after SQL-shaped prefix: %v", err)
	}
}

func TestKVKeysUsesMemstoreBytewiseOrdering(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := []string{"a-1", "a.1", "a/1", "a0"}
	for _, key := range []string{"a0", "a/1", "a.1", "a-1"} {
		if _, err := store.KV.Put(ctx, key, 0, []byte{}); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	got, err := store.KV.Keys(ctx, "a")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Keys = %v, want bytewise %v", got, want)
	}
}

func TestOperationErrorsDoNotDiscloseDSNOrCredential(t *testing.T) {
	store := newConformanceStore(t)
	store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := store.KV.Get(ctx, "sessions/closed")
	if err == nil {
		t.Fatal("Get after Close = nil, want error")
	}
	for _, secret := range []string{":pgstore@", "postgres://postgres:pgstore@127.0.0.1:5432/pgstore", "127.0.0.1:5432"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("operation error disclosed %q: %v", secret, err)
		}
	}
}
