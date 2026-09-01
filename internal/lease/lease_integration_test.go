//go:build integration

package lease

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/storage"
)

var testPrefixID atomic.Uint64

func TestLeaseAcquireHeldReleaseAndLaterEpoch(t *testing.T) {
	store, pool, table := newTestStore(t, 5*time.Second, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := store.Acquire(ctx, "sessions/one")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if first == nil || first.Epoch() != 1 || isClosed(first.Lost()) {
		t.Fatalf("first grant = (%v, epoch %d, lost %t), want live epoch 1", first, first.Epoch(), isClosed(first.Lost()))
	}
	_, err = store.Acquire(ctx, "sessions/one")
	var held *storage.LeaseHeldError
	if !errors.As(err, &held) || held.Name != "sessions/one" || held.HolderEpoch != 1 {
		t.Fatalf("second Acquire = %#v, want held at epoch 1", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !isClosed(first.Lost()) {
		t.Fatal("Lost remained open after Release")
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	second, err := store.Acquire(ctx, "sessions/one")
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if second.Epoch() != 2 {
		t.Fatalf("later epoch = %d, want 2", second.Epoch())
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	_, err = store.Acquire(ctx, "sessions/one")
	if !errors.As(err, &held) || held.HolderEpoch != 2 {
		t.Fatalf("Acquire after stale Release = %#v, want held at epoch 2", err)
	}

	var rows int
	var epoch, revision int64
	var holderPresent bool
	if err := pool.QueryRow(ctx, "SELECT count(*), max(epoch), max(revision), bool_or(holder IS NOT NULL) FROM "+table+" WHERE name = $1", "sessions/one").Scan(&rows, &epoch, &revision, &holderPresent); err != nil {
		t.Fatalf("read persistent row: %v", err)
	}
	if rows != 1 || epoch != 2 || revision < 3 || !holderPresent {
		t.Fatalf("persistent row = rows %d epoch %d revision %d holder %t", rows, epoch, revision, holderPresent)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

func TestLeaseConcurrentFirstAcquireHasOneWinner(t *testing.T) {
	store, _, _ := newTestStore(t, 5*time.Second, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const contenders = 12
	start := make(chan struct{})
	type result struct {
		lease storage.Lease
		err   error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := store.Acquire(ctx, "sessions/race")
			results <- result{lease: lease, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner storage.Lease
	var successes, held int
	for result := range results {
		if result.err == nil {
			successes++
			winner = result.lease
			continue
		}
		var heldErr *storage.LeaseHeldError
		if !errors.As(result.err, &heldErr) || heldErr.HolderEpoch != 1 {
			t.Errorf("loser = %v, want LeaseHeldError at epoch 1", result.err)
		}
		held++
	}
	if successes != 1 || held != contenders-1 {
		t.Fatalf("concurrent Acquire = %d successes and %d held, want 1 and %d", successes, held, contenders-1)
	}
	if err := winner.Release(ctx); err != nil {
		t.Fatalf("winner Release: %v", err)
	}
}

func TestLeaseConcurrentExpiredTakeoverHasOneWinner(t *testing.T) {
	store, pool, table := newTestStore(t, time.Hour, 30*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := store.Acquire(ctx, "sessions/expired-race")
	if err != nil {
		t.Fatalf("seed Acquire: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE "+table+" SET expires_at = clock_timestamp() WHERE name = $1", "sessions/expired-race"); err != nil {
		t.Fatalf("expire seed: %v", err)
	}

	const contenders = 12
	start := make(chan struct{})
	type result struct {
		lease storage.Lease
		err   error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := store.Acquire(ctx, "sessions/expired-race")
			results <- result{lease: lease, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var winner storage.Lease
	var successes int
	for result := range results {
		if result.err == nil {
			successes++
			winner = result.lease
			continue
		}
		var held *storage.LeaseHeldError
		if !errors.As(result.err, &held) || held.HolderEpoch != first.Epoch()+1 {
			t.Errorf("takeover loser = %v, want held at epoch %d", result.err, first.Epoch()+1)
		}
	}
	if successes != 1 || winner == nil {
		t.Fatalf("expired takeover = %d successes, want exactly one", successes)
	}
	if winner.Epoch() != first.Epoch()+1 {
		t.Fatalf("expired takeover epoch = %d, want %d", winner.Epoch(), first.Epoch()+1)
	}
	if err := winner.Release(ctx); err != nil {
		t.Fatalf("winner Release: %v", err)
	}
}

func TestLeaseRenewsUntilReleased(t *testing.T) {
	store, _, _ := newTestStore(t, 180*time.Millisecond, 40*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/renew")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if isClosed(lease.Lost()) {
		t.Fatal("Lost closed while renewals should keep the grant live")
	}
	_, err = store.Acquire(ctx, "sessions/renew")
	var held *storage.LeaseHeldError
	if !errors.As(err, &held) || held.HolderEpoch != lease.Epoch() {
		t.Fatalf("Acquire after renewal window = %v, want held at epoch %d", err, lease.Epoch())
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestLeaseExpiryClosesLostAndLaterGrantAdvancesEpoch(t *testing.T) {
	store, pool, table := newTestStore(t, 250*time.Millisecond, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := store.Acquire(ctx, "sessions/expire")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	grant := first.(*Lease)
	if _, err := pool.Exec(ctx, "UPDATE "+table+" SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE name = $1 AND epoch = $2 AND holder = $3", grant.name, int64(grant.epoch), grant.holder); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	waitClosed(t, first.Lost(), time.Second, "Lost after database expiry")
	second, err := store.Acquire(ctx, "sessions/expire")
	if err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	if second.Epoch() != first.Epoch()+1 {
		t.Fatalf("later epoch = %d, want %d", second.Epoch(), first.Epoch()+1)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("Release later grant: %v", err)
	}
}

func TestLeaseLocalDeadlineClosesLostBeforeRenewTick(t *testing.T) {
	store, _, _ := newTestStore(t, 80*time.Millisecond, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/local-expiry")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	waitClosed(t, lease.Lost(), 500*time.Millisecond, "Lost at the locally tracked database expiry")
}

func TestLeaseLocalDeadlineAccountsForCommitLatency(t *testing.T) {
	store, _, _ := newTestStore(t, 60*time.Millisecond, time.Second)
	store.commit = func(ctx context.Context, tx pgx.Tx) error {
		err := tx.Commit(ctx)
		time.Sleep(150 * time.Millisecond)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/commit-latency")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	waitClosed(t, lease.Lost(), 30*time.Millisecond, "Lost after commit latency crossed the database expiry")
}

func TestLeaseExpiryComparisonAtAndAroundThreshold(t *testing.T) {
	for _, test := range []struct {
		name       string
		expression string
		wantHeld   bool
	}{
		{name: "one below", expression: "clock_timestamp() - interval '1 microsecond'"},
		{name: "equal", expression: "clock_timestamp()"},
		{name: "one above", expression: "clock_timestamp() + interval '1 second'", wantHeld: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, pool, table := newTestStore(t, time.Hour, 30*time.Minute)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			first, err := store.Acquire(ctx, "sessions/threshold")
			if err != nil {
				t.Fatalf("first Acquire: %v", err)
			}
			grant := first.(*Lease)
			if _, err := pool.Exec(ctx, "UPDATE "+table+" SET expires_at = "+test.expression+" WHERE name = $1 AND epoch = $2 AND holder = $3", grant.name, int64(grant.epoch), grant.holder); err != nil {
				t.Fatalf("set threshold: %v", err)
			}
			second, err := store.Acquire(ctx, "sessions/threshold")
			if test.wantHeld {
				var held *storage.LeaseHeldError
				if !errors.As(err, &held) || held.HolderEpoch != first.Epoch() {
					t.Fatalf("Acquire = %v, want held at epoch %d", err, first.Epoch())
				}
				return
			}
			if err != nil {
				t.Fatalf("Acquire at expired boundary: %v", err)
			}
			if second.Epoch() != first.Epoch()+1 {
				t.Fatalf("later epoch = %d, want %d", second.Epoch(), first.Epoch()+1)
			}
			if err := second.Release(ctx); err != nil {
				t.Fatalf("Release: %v", err)
			}
		})
	}
}

func TestLeaseCanceledAcquireDoesNotWrite(t *testing.T) {
	store, pool, table := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	_, err := store.Acquire(ctx, "sessions/canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire with canceled context = %v, want context.Canceled", err)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
	defer checkCancel()
	var rows int
	if err := pool.QueryRow(checkCtx, "SELECT count(*) FROM "+table+" WHERE name = $1", "sessions/canceled").Scan(&rows); err != nil {
		t.Fatalf("count canceled rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("canceled Acquire wrote %d rows, want 0", rows)
	}
}

func TestLeaseCanceledReleaseCanBeRetried(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/release-retry")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	canceled, cancelRelease := context.WithTimeout(context.Background(), time.Second)
	cancelRelease()
	if err := lease.Release(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Release = %v, want context.Canceled", err)
	}
	if !isClosed(lease.Lost()) {
		t.Fatal("Lost remained open after canceled Release")
	}
	_, err = store.Acquire(ctx, "sessions/release-retry")
	var held *storage.LeaseHeldError
	if !errors.As(err, &held) || held.HolderEpoch != lease.Epoch() {
		t.Fatalf("Acquire before release retry = %v, want held at epoch %d", err, lease.Epoch())
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("retry Release: %v", err)
	}
	later, err := store.Acquire(ctx, "sessions/release-retry")
	if err != nil {
		t.Fatalf("Acquire after release retry: %v", err)
	}
	if later.Epoch() != lease.Epoch()+1 {
		t.Fatalf("later epoch = %d, want %d", later.Epoch(), lease.Epoch()+1)
	}
	if err := later.Release(ctx); err != nil {
		t.Fatalf("later Release: %v", err)
	}
}

func TestLeaseAcquireAppliesTransactionLocalLockTimeout(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	store, pool, table := newTestStore(t, time.Second, 200*time.Millisecond)
	store.statementTimeout = 250 * time.Millisecond
	store.lockTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/lock-timeout")
	if err != nil {
		t.Fatalf("seed Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("seed Release: %v", err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, "UPDATE "+table+" SET revision = revision WHERE name = $1", "sessions/lock-timeout"); err != nil {
		t.Fatalf("lock lease row: %v", err)
	}
	started := time.Now()
	_, err = store.Acquire(ctx, "sessions/lock-timeout")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Acquire through blocked row = nil error")
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("blocked Acquire took %s, want transaction-local 50ms lock timeout", elapsed)
	}
}

func TestLeaseEpochAndHolderFencesAtAndAroundThreshold(t *testing.T) {
	store, pool, table := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := store.Acquire(ctx, "sessions/fence")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	secondLease, err := store.Acquire(ctx, "sessions/fence")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	second := secondLease.(*Lease)
	var initialRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE name = $1", second.name).Scan(&initialRevision); err != nil {
		t.Fatalf("read initial revision: %v", err)
	}
	for _, epoch := range []int64{second.epoch - 1, second.epoch + 1} {
		forged := &Lease{store: store, name: second.name, epoch: epoch, holder: bytes.Clone(second.holder)}
		if _, err := store.renewRow(ctx, forged); err != pgx.ErrNoRows {
			t.Fatalf("renew at epoch %d = %v, want pgx.ErrNoRows", epoch, err)
		}
	}
	forgedHolder := &Lease{store: store, name: second.name, epoch: second.epoch, holder: bytes.Repeat([]byte{7}, holderTokenBytes)}
	if _, err := store.renewRow(ctx, forgedHolder); err != pgx.ErrNoRows {
		t.Fatalf("renew with wrong holder = %v, want pgx.ErrNoRows", err)
	}
	var fencedRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE name = $1", second.name).Scan(&fencedRevision); err != nil {
		t.Fatalf("read fenced revision: %v", err)
	}
	if fencedRevision != initialRevision {
		t.Fatalf("stale writers changed revision from %d to %d", initialRevision, fencedRevision)
	}
	if _, err := store.renewRow(ctx, second); err != nil {
		t.Fatalf("renew at exact epoch+holder threshold: %v", err)
	}
	var exactRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE name = $1", second.name).Scan(&exactRevision); err != nil {
		t.Fatalf("read exact revision: %v", err)
	}
	if exactRevision != initialRevision+1 {
		t.Fatalf("exact writer revision = %d, want %d", exactRevision, initialRevision+1)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestLeaseReleaseRequiresExactEpochAndHolder(t *testing.T) {
	store, pool, table := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/release-fence")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	current := lease.(*Lease)
	var initialRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE name = $1", current.name).Scan(&initialRevision); err != nil {
		t.Fatalf("read initial revision: %v", err)
	}
	wrongEpochBelow := &Lease{store: store, name: current.name, epoch: current.epoch - 1, holder: bytes.Clone(current.holder)}
	wrongEpochAbove := &Lease{store: store, name: current.name, epoch: current.epoch + 1, holder: bytes.Clone(current.holder)}
	wrongHolder := &Lease{store: store, name: current.name, epoch: current.epoch, holder: bytes.Repeat([]byte{4}, holderTokenBytes)}
	for label, forged := range map[string]*Lease{"wrong epoch below": wrongEpochBelow, "wrong epoch above": wrongEpochAbove, "wrong holder": wrongHolder} {
		if err := store.releaseRow(ctx, forged); err != nil {
			t.Fatalf("%s release: %v", label, err)
		}
	}
	var fencedRevision int64
	var holderPresent bool
	if err := pool.QueryRow(ctx, "SELECT revision, holder IS NOT NULL FROM "+table+" WHERE name = $1", current.name).Scan(&fencedRevision, &holderPresent); err != nil {
		t.Fatalf("read fenced row: %v", err)
	}
	if fencedRevision != initialRevision || !holderPresent {
		t.Fatalf("stale release changed row to revision %d holder %t, want revision %d held", fencedRevision, holderPresent, initialRevision)
	}
	if err := current.Release(ctx); err != nil {
		t.Fatalf("exact Release: %v", err)
	}
	var exactRevision int64
	if err := pool.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE name = $1", current.name).Scan(&exactRevision); err != nil {
		t.Fatalf("read exact release revision: %v", err)
	}
	if exactRevision != initialRevision+1 {
		t.Fatalf("exact release revision = %d, want %d", exactRevision, initialRevision+1)
	}
}

func TestLeaseAmbiguousRenewClosesLostUnlessAuthoritativeReadProvesOwnership(t *testing.T) {
	t.Run("later epoch is lost", func(t *testing.T) {
		store, pool, table := newTestStore(t, 500*time.Millisecond, 40*time.Millisecond)
		store.renew = func(ctx context.Context, lease *Lease) (renewal, error) {
			if _, err := pool.Exec(ctx, "UPDATE "+table+" SET epoch = epoch + 1, holder = $2, expires_at = clock_timestamp() + interval '1 second', revision = revision + 1 WHERE name = $1", lease.name, bytes.Repeat([]byte{9}, holderTokenBytes)); err != nil {
				t.Fatalf("install later epoch: %v", err)
			}
			return renewal{}, errors.New("renew acknowledgement lost")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lease, err := store.Acquire(ctx, "sessions/ambiguous-lost")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		waitClosed(t, lease.Lost(), time.Second, "Lost after ambiguous renewal observed later epoch")
	})

	t.Run("committed renewal is proved", func(t *testing.T) {
		store, _, _ := newTestStore(t, 500*time.Millisecond, 40*time.Millisecond)
		var calls atomic.Int32
		store.renew = func(ctx context.Context, lease *Lease) (renewal, error) {
			result, err := store.renewRow(ctx, lease)
			if err != nil {
				return renewal{}, err
			}
			if calls.Add(1) == 1 {
				return result, errors.New("renew acknowledgement lost")
			}
			return result, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lease, err := store.Acquire(ctx, "sessions/ambiguous-proved")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		time.Sleep(180 * time.Millisecond)
		if isClosed(lease.Lost()) {
			t.Fatal("Lost closed although authoritative reread proved the committed renewal")
		}
		if calls.Load() < 2 {
			t.Fatalf("renew calls = %d, want at least 2", calls.Load())
		}
		if err := lease.Release(ctx); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

func TestLeaseAcquireResolvesCommittedLostAcknowledgement(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	store.commit = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("commit acknowledgement lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/acquire-lost-ack")
	if err != nil {
		t.Fatalf("Acquire after committed lost acknowledgement: %v", err)
	}
	if lease.Epoch() != 1 || isClosed(lease.Lost()) {
		t.Fatalf("resolved lease = epoch %d lost %t, want live epoch 1", lease.Epoch(), isClosed(lease.Lost()))
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestLeaseAcquireRejectsUncommittedLostAcknowledgement(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	store.commit = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return errors.New("commit acknowledgement lost before commit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if lease, err := store.Acquire(ctx, "sessions/acquire-uncommitted"); err == nil || lease != nil {
		t.Fatalf("Acquire after rolled-back acknowledgement = (%v, %v), want no lease and error", lease, err)
	}
	store.commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	lease, err := store.Acquire(ctx, "sessions/acquire-uncommitted")
	if err != nil {
		t.Fatalf("Acquire after rolled-back attempt: %v", err)
	}
	if lease.Epoch() != 1 {
		t.Fatalf("epoch after rolled-back attempt = %d, want 1", lease.Epoch())
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestLeaseReleaseResolvesCommittedLostAcknowledgement(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	store.release = func(ctx context.Context, lease *Lease) error {
		if err := store.releaseRow(ctx, lease); err != nil {
			return err
		}
		return errors.New("release acknowledgement lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/release-lost-ack")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release after committed lost acknowledgement: %v", err)
	}
	if !isClosed(lease.Lost()) {
		t.Fatal("Lost remained open after resolved Release")
	}
	later, err := store.Acquire(ctx, "sessions/release-lost-ack")
	if err != nil {
		t.Fatalf("Acquire after resolved Release: %v", err)
	}
	if later.Epoch() != lease.Epoch()+1 {
		t.Fatalf("later epoch = %d, want %d", later.Epoch(), lease.Epoch()+1)
	}
	if err := later.Release(ctx); err != nil {
		t.Fatalf("later Release: %v", err)
	}
}

func TestLeaseReleaseRejectsUncommittedLostAcknowledgementAndRetries(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/release-uncommitted")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	store.release = func(context.Context, *Lease) error {
		return errors.New("release acknowledgement lost before commit")
	}
	if err := lease.Release(ctx); err == nil {
		t.Fatal("Release after uncommitted acknowledgement = nil error")
	}
	if !isClosed(lease.Lost()) {
		t.Fatal("Lost remained open after failed Release")
	}
	var held *storage.LeaseHeldError
	if _, err := store.Acquire(ctx, "sessions/release-uncommitted"); !errors.As(err, &held) || held.HolderEpoch != lease.Epoch() {
		t.Fatalf("Acquire before retry = %v, want held at epoch %d", err, lease.Epoch())
	}
	store.release = store.releaseRow
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("retry Release: %v", err)
	}
	later, err := store.Acquire(ctx, "sessions/release-uncommitted")
	if err != nil {
		t.Fatalf("Acquire after retry: %v", err)
	}
	if later.Epoch() != lease.Epoch()+1 {
		t.Fatalf("later epoch = %d, want %d", later.Epoch(), lease.Epoch()+1)
	}
	if err := later.Release(ctx); err != nil {
		t.Fatalf("later Release: %v", err)
	}
}

func TestLeaseConcurrentReleaseExecutesOneDatabaseRelease(t *testing.T) {
	store, _, _ := newTestStore(t, time.Second, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/concurrent-release")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var calls atomic.Int32
	store.release = func(ctx context.Context, lease *Lease) error {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return store.releaseRow(ctx, lease)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- lease.Release(ctx)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Release: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("database release calls = %d, want 1", calls.Load())
	}
}

func TestLeaseSurvivesPhysicalConnectionTurnover(t *testing.T) {
	store, pool, _ := newTestStore(t, 180*time.Millisecond, 40*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/connection-turnover")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Reset()
	time.Sleep(350 * time.Millisecond)
	if isClosed(lease.Lost()) {
		t.Fatal("Lost closed after pool discarded every physical connection")
	}
	_, err = store.Acquire(ctx, "sessions/connection-turnover")
	var held *storage.LeaseHeldError
	if !errors.As(err, &held) || held.HolderEpoch != lease.Epoch() {
		t.Fatalf("Acquire after connection turnover = %v, want held at epoch %d", err, lease.Epoch())
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestLeaseDatabaseRestartClosesLost(t *testing.T) {
	container := os.Getenv("PGSTORE_TEST_DOCKER_CONTAINER")
	if container == "" {
		t.Skip("PGSTORE_TEST_DOCKER_CONTAINER is not set")
	}
	store, pool, _ := newTestStore(t, 250*time.Millisecond, 40*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := store.Acquire(ctx, "sessions/restart")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	command := exec.CommandContext(ctx, "docker", "restart", container)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v: %s", err, output)
	}
	waitClosed(t, lease.Lost(), 2*time.Second, "Lost after database restart crossed expiry")
	for ctx.Err() == nil {
		if err := pool.Ping(ctx); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("database did not become ready after restart")
}

func newTestStore(t *testing.T, ttl, renew time.Duration) (*Store, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	prefix := fmt.Sprintf("lease%x_", testPrefixID.Add(1))
	table := `"looprig"."` + prefix + `leases"`
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS looprig; CREATE TABLE `+table+` (
name text PRIMARY KEY,
epoch bigint NOT NULL CHECK (epoch >= 0),
holder bytea,
expires_at timestamptz,
revision bigint NOT NULL CHECK (revision >= 0),
CHECK ((holder IS NULL) = (expires_at IS NULL)))`); err != nil {
		t.Fatalf("create lease table: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, "DROP TABLE IF EXISTS "+table)
	})
	store := New(pool, "looprig", prefix, ttl, renew)
	t.Cleanup(store.Close)
	return store, pool, table
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatal(what + " remained open")
	}
}
