// Package lease contains the PostgreSQL renewable epoch-lease adapter.
package lease

import (
	"bytes"
	"context"
	"crypto/rand"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
)

const holderTokenBytes = 32

type Store struct {
	pool             *pgxpool.Pool
	schema           string
	tablePrefix      string
	leaseTTL         time.Duration
	renewInterval    time.Duration
	statementTimeout time.Duration
	lockTimeout      time.Duration
	renew            func(context.Context, *Lease) (renewal, error)
	commit           func(context.Context, pgx.Tx) error
	release          func(context.Context, *Lease) error

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	leases map[*Lease]struct{}
}

func New(pool *pgxpool.Pool, schema, tablePrefix string, leaseTTL, renewInterval time.Duration, operationTimeouts ...time.Duration) *Store {
	ctx, cancel := context.WithCancel(context.Background())
	statementTimeout, lockTimeout := 30*time.Second, 5*time.Second
	if len(operationTimeouts) == 2 {
		statementTimeout, lockTimeout = operationTimeouts[0], operationTimeouts[1]
	}
	store := &Store{
		pool: pool, schema: schema, tablePrefix: tablePrefix,
		leaseTTL: leaseTTL, renewInterval: renewInterval,
		statementTimeout: statementTimeout, lockTimeout: lockTimeout,
		ctx: ctx, cancel: cancel, leases: make(map[*Lease]struct{}),
	}
	store.renew = store.renewRow
	store.commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	store.release = store.releaseRow
	return store
}

type renewal struct {
	remaining time.Duration
}

func (s *Store) renewRow(ctx context.Context, lease *Lease) (renewal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return renewal{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return renewal{}, err
	}
	var expiresAt, databaseNow time.Time
	proofStarted := time.Now()
	err = tx.QueryRow(ctx, "UPDATE "+s.table()+" SET expires_at = clock_timestamp() + $4::bigint * interval '1 millisecond', revision = revision + 1 WHERE name = $1 AND epoch = $2 AND holder = $3 AND expires_at > clock_timestamp() RETURNING expires_at, clock_timestamp()", lease.name, lease.epoch, lease.holder, s.leaseTTL.Milliseconds()).Scan(&expiresAt, &databaseNow)
	if err != nil {
		return renewal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return renewal{}, err
	}
	return renewal{remaining: conservativeRemaining(proofStarted, expiresAt, databaseNow)}, nil
}

func (s *Store) releaseRow(ctx context.Context, lease *Lease) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE "+s.table()+" SET holder = NULL, expires_at = NULL, revision = revision + 1 WHERE name = $1 AND epoch = $2 AND holder = $3", lease.name, lease.epoch, lease.holder); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	if err := guard.RequireDeadline(ctx, "Leaser.Acquire"); err != nil {
		return nil, err
	}
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	holder := make([]byte, holderTokenBytes)
	if _, err := rand.Read(holder); err != nil {
		return nil, pginternal.RedactedError("lease holder token")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO "+s.table()+" (name, epoch, revision) VALUES ($1, 0, 0) ON CONFLICT (name) DO NOTHING", name); err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}
	var epoch int64
	var currentHolder []byte
	var expiresAt *time.Time
	var databaseNow time.Time
	if err = tx.QueryRow(ctx, "SELECT epoch, holder, expires_at, clock_timestamp() FROM "+s.table()+" WHERE name = $1 FOR UPDATE", name).Scan(&epoch, &currentHolder, &expiresAt, &databaseNow); err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}
	if epoch < 0 {
		return nil, pginternal.RedactedError("lease epoch invalid")
	}
	if currentHolder != nil && expiresAt != nil && expiresAt.After(databaseNow) {
		return nil, &storage.LeaseHeldError{Name: name, HolderEpoch: uint64(epoch)}
	}
	if epoch == math.MaxInt64 {
		return nil, pginternal.RedactedError("lease epoch exhausted")
	}
	epoch++
	proofStarted := time.Now()
	if err = tx.QueryRow(ctx, "UPDATE "+s.table()+" SET epoch = $2, holder = $3, expires_at = clock_timestamp() + $4::bigint * interval '1 millisecond', revision = revision + 1 WHERE name = $1 RETURNING expires_at, clock_timestamp()", name, epoch, holder, s.leaseTTL.Milliseconds()).Scan(&expiresAt, &databaseNow); err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}
	if err = s.commit(ctx, tx); err != nil {
		return s.resolveAcquire(name, epoch, holder)
	}
	return s.newLease(name, epoch, holder, conservativeRemaining(proofStarted, *expiresAt, databaseNow)), nil
}

func (s *Store) newLease(name string, epoch int64, holder []byte, remaining time.Duration) *Lease {
	lease := &Lease{
		store: s, name: name, epoch: epoch, holder: holder,
		safeUntil: time.Now().Add(remaining),
		lost:      make(chan struct{}), done: make(chan struct{}),
	}
	s.register(lease)
	go lease.renewLoop()
	return lease
}

func (s *Store) resolveAcquire(name string, epoch int64, holder []byte) (storage.Lease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var storedEpoch int64
	var storedHolder []byte
	var expiresAt *time.Time
	var databaseNow time.Time
	proofStarted := time.Now()
	if err := s.pool.QueryRow(ctx, "SELECT epoch, holder, expires_at, clock_timestamp() FROM "+s.table()+" WHERE name = $1", name).Scan(&storedEpoch, &storedHolder, &expiresAt, &databaseNow); err != nil {
		return nil, pginternal.RedactedError("lease acquire outcome resolution")
	}
	if storedEpoch == epoch && bytes.Equal(storedHolder, holder) && expiresAt != nil && expiresAt.After(databaseNow) {
		return s.newLease(name, epoch, holder, conservativeRemaining(proofStarted, *expiresAt, databaseNow)), nil
	}
	if storedHolder != nil && expiresAt != nil && expiresAt.After(databaseNow) {
		if storedEpoch < 0 {
			return nil, pginternal.RedactedError("lease epoch invalid")
		}
		return nil, &storage.LeaseHeldError{Name: name, HolderEpoch: uint64(storedEpoch)}
	}
	return nil, pginternal.RedactedError("lease acquire outcome resolution")
}

type Lease struct {
	store  *Store
	name   string
	epoch  int64
	holder []byte

	mu              sync.Mutex
	safeUntil       time.Time
	released        bool
	releaseComplete bool
	releaseMu       sync.Mutex
	lost            chan struct{}
	lostOnce        sync.Once
	done            chan struct{}
	doneOnce        sync.Once
}

func (l *Lease) Epoch() uint64 {
	if l.epoch < 0 {
		return 0
	}
	return uint64(l.epoch)
}

func (l *Lease) Lost() <-chan struct{} { return l.lost }

func (l *Lease) Release(ctx context.Context) error {
	if err := guard.RequireDeadline(ctx, "Lease.Release"); err != nil {
		return err
	}
	l.releaseMu.Lock()
	defer l.releaseMu.Unlock()
	l.mu.Lock()
	if l.releaseComplete {
		l.mu.Unlock()
		return nil
	}
	firstAttempt := !l.released
	if firstAttempt {
		l.released = true
	}
	l.mu.Unlock()
	if firstAttempt {
		l.stop()
		l.markLost()
		l.store.unregister(l)
	}

	err := l.store.release(ctx, l)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err := l.store.resolveRelease(l); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.releaseComplete = true
	l.mu.Unlock()
	return nil
}

func (s *Store) resolveRelease(lease *Lease) error {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var epoch int64
	var holder []byte
	var expiresAt *time.Time
	var databaseNow time.Time
	err := s.pool.QueryRow(ctx, "SELECT epoch, holder, expires_at, clock_timestamp() FROM "+s.table()+" WHERE name = $1", lease.name).Scan(&epoch, &holder, &expiresAt, &databaseNow)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return pginternal.RedactedError("lease release outcome resolution")
	}
	if epoch != lease.epoch || !bytes.Equal(holder, lease.holder) || expiresAt == nil || !expiresAt.After(databaseNow) {
		return nil
	}
	return pginternal.RedactedError("lease release outcome resolution")
}

func (l *Lease) markLost() {
	l.lostOnce.Do(func() { close(l.lost) })
}

func (l *Lease) stop() {
	l.doneOnce.Do(func() { close(l.done) })
}

func (l *Lease) renewLoop() {
	defer l.store.unregister(l)
	for {
		l.mu.Lock()
		safeUntil := l.safeUntil
		released := l.released
		l.mu.Unlock()
		if released {
			return
		}
		remaining := time.Until(safeUntil)
		if remaining <= 0 {
			l.markLost()
			return
		}
		renewTimer := time.NewTimer(l.store.renewInterval)
		expiryTimer := time.NewTimer(remaining)
		select {
		case <-l.done:
			stopTimer(renewTimer)
			stopTimer(expiryTimer)
			return
		case <-l.store.ctx.Done():
			stopTimer(renewTimer)
			stopTimer(expiryTimer)
			l.markLost()
			return
		case <-expiryTimer.C:
			stopTimer(renewTimer)
			l.markLost()
			return
		case <-renewTimer.C:
			stopTimer(expiryTimer)
		}

		opCtx, cancel := context.WithDeadline(l.store.ctx, safeUntil)
		result, err := l.store.renew(opCtx, l)
		cancel()
		if err != nil {
			proofCtx, proofCancel := context.WithDeadline(l.store.ctx, safeUntil)
			result, err = l.store.observe(proofCtx, l)
			proofCancel()
			if err != nil {
				l.markLost()
				return
			}
		}
		if result.remaining <= 0 {
			l.markLost()
			return
		}
		l.mu.Lock()
		l.safeUntil = time.Now().Add(result.remaining)
		l.mu.Unlock()
	}
}

func (s *Store) observe(ctx context.Context, lease *Lease) (renewal, error) {
	var epoch int64
	var holder []byte
	var expiresAt *time.Time
	var databaseNow time.Time
	proofStarted := time.Now()
	if err := s.pool.QueryRow(ctx, "SELECT epoch, holder, expires_at, clock_timestamp() FROM "+s.table()+" WHERE name = $1", lease.name).Scan(&epoch, &holder, &expiresAt, &databaseNow); err != nil {
		return renewal{}, err
	}
	if epoch != lease.epoch || !bytes.Equal(holder, lease.holder) || expiresAt == nil || !expiresAt.After(databaseNow) {
		return renewal{}, &storage.LeaseLostError{Name: lease.name, Epoch: lease.Epoch()}
	}
	return renewal{remaining: conservativeRemaining(proofStarted, *expiresAt, databaseNow)}, nil
}

func conservativeRemaining(proofStarted, expiresAt, databaseNow time.Time) time.Duration {
	return expiresAt.Sub(databaseNow) - time.Since(proofStarted)
}

func (s *Store) register(lease *Lease) {
	s.mu.Lock()
	s.leases[lease] = struct{}{}
	s.mu.Unlock()
}

func (s *Store) unregister(lease *Lease) {
	s.mu.Lock()
	delete(s.leases, lease)
	s.mu.Unlock()
}

func (s *Store) Close() {
	s.cancel()
	s.mu.Lock()
	leases := make([]*Lease, 0, len(s.leases))
	for lease := range s.leases {
		leases = append(leases, lease)
	}
	s.mu.Unlock()
	for _, lease := range leases {
		lease.stop()
		lease.markLost()
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (s *Store) table() string {
	return pginternal.Qualified(s.schema, s.tablePrefix+"leases")
}

func leaseFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

var _ storage.Leaser = (*Store)(nil)
var _ storage.Lease = (*Lease)(nil)
