// Package orderedindex implements Storage's bounded PostgreSQL OrderedIndex.
package orderedindex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
)

const (
	orderedScopesSuffix  = "ordered_scopes"
	orderedRecordsSuffix = "ordered_records"
	cursorVersion        = 1
	maxCursorEncoded     = 4096
	maxCursorDecoded     = 3072
)

type Store struct {
	pool             *pgxpool.Pool
	schema           string
	tablePrefix      string
	statementTimeout time.Duration
	lockTimeout      time.Duration
	commit           func(context.Context, pgx.Tx) error
}

func New(pool *pgxpool.Pool, schema, tablePrefix string, operationTimeouts ...time.Duration) *Store {
	statementTimeout, lockTimeout := 30*time.Second, 5*time.Second
	if len(operationTimeouts) == 2 {
		statementTimeout, lockTimeout = operationTimeouts[0], operationTimeouts[1]
	}
	return &Store{pool: pool, schema: schema, tablePrefix: tablePrefix, statementTimeout: statementTimeout, lockTimeout: lockTimeout, commit: func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }}
}

func (s *Store) Get(ctx context.Context, id storage.OrderedID) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Get"); err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := storage.ValidateOrderedID(id); err != nil {
		return storage.OrderedRecord{}, err
	}
	record, err := s.get(ctx, id)
	if err == pgx.ErrNoRows {
		return storage.OrderedRecord{}, &storage.OrderedRecordNotFoundError{ID: id}
	}
	if err != nil {
		return storage.OrderedRecord{}, failure(ctx, "ordered get")
	}
	return record, nil
}

func (s *Store) Create(ctx context.Context, id storage.OrderedID, rankingScope string, value []byte, rank storage.Rank, due storage.Due) (storage.OrderedRecord, bool, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Create"); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if err := storage.ValidateOrderedID(id); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if existing, err := s.get(ctx, id); err == nil {
		return existing, false, nil
	} else if err != pgx.ErrNoRows {
		return storage.OrderedRecord{}, false, failure(ctx, "ordered create lookup")
	}
	if err := validateCandidate(rankingScope, value, due); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	for attempt := 0; attempt < pginternal.Attempts(); attempt++ {
		record, created, err := s.createOnce(ctx, id, rankingScope, value, rank, due)
		if err == nil {
			return record, created, nil
		}
		var commitErr *commitOutcomeError
		if errors.As(err, &commitErr) {
			return s.resolveCreate(id, record, commitErr.cause)
		}
		if isContractError(err) {
			return storage.OrderedRecord{}, false, err
		}
		if pginternal.Retryable(err) && ctx.Err() == nil && attempt+1 < pginternal.Attempts() {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return storage.OrderedRecord{}, false, ctxErr
		}
		return storage.OrderedRecord{}, false, failure(ctx, "ordered create")
	}
	panic("unreachable")
}

func (s *Store) createOnce(ctx context.Context, id storage.OrderedID, rankingScope string, value []byte, rank storage.Rank, due storage.Due) (storage.OrderedRecord, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storage.OrderedRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if preexisting, readErr := s.getFrom(ctx, tx, id, ""); readErr == nil {
		return preexisting, false, nil
	} else if readErr != pgx.ErrNoRows {
		return storage.OrderedRecord{}, false, readErr
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+s.scopesTable()+" (namespace, ordering_scope, next_order) VALUES ($1, $2, 0) ON CONFLICT (namespace, ordering_scope) DO NOTHING", id.Namespace, id.OrderingScope); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	// The counter row is the per-order-scope serialization point. Recheck the
	// identity only after acquiring it: concurrent retries then observe the
	// winner and return without advancing the counter.
	var orderText string
	err = tx.QueryRow(ctx, "SELECT next_order::text FROM "+s.scopesTable()+" WHERE namespace = $1 AND ordering_scope = $2 FOR UPDATE", id.Namespace, id.OrderingScope).Scan(&orderText)
	if err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if lockedExisting, readErr := s.getFrom(ctx, tx, id, ""); readErr == nil {
		return lockedExisting, false, nil
	} else if readErr != pgx.ErrNoRows {
		return storage.OrderedRecord{}, false, readErr
	}
	currentOrder, err := parseUint(orderText)
	if err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if currentOrder == math.MaxUint64 {
		return storage.OrderedRecord{}, false, &orderExhaustedError{}
	}
	err = tx.QueryRow(ctx, "UPDATE "+s.scopesTable()+" SET next_order = next_order + 1 WHERE namespace = $1 AND ordering_scope = $2 RETURNING next_order::text", id.Namespace, id.OrderingScope).Scan(&orderText)
	if err == pgx.ErrNoRows {
		return storage.OrderedRecord{}, false, &orderExhaustedError{}
	}
	if err != nil {
		return storage.OrderedRecord{}, false, err
	}
	order, err := parseUint(orderText)
	if err != nil || order == 0 {
		return storage.OrderedRecord{}, false, pginternal.RedactedError("ordered order decode")
	}
	storedValue, valueIsNil := databaseValue(value)
	record := storage.OrderedRecord{ID: id, RankingScope: rankingScope, Revision: 1, Order: order, Value: cloneValue(value), Rank: rank, Due: due}
	_, err = tx.Exec(ctx, "INSERT INTO "+s.recordsTable()+" (namespace, ordering_scope, stable_key, ranking_scope, revision, order_id, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted) VALUES ($1, $2, $3, $4, 1, $5::numeric, $6, $7, $8, $9, $10, $11, false)", id.Namespace, id.OrderingScope, string(id.StableKey), rankingScope, orderText, storedValue, valueIsNil, rank.Ranked, rank.Value, int16(due.State), due.UnixMillis)
	if err != nil {
		return storage.OrderedRecord{}, false, err
	}
	if err := s.commit(ctx, tx); err != nil {
		if pginternal.Retryable(err) {
			return storage.OrderedRecord{}, false, err
		}
		return record, false, &commitOutcomeError{cause: safeCause(ctx, "ordered create commit")}
	}
	return record, true, nil
}

func (s *Store) Update(ctx context.Context, id storage.OrderedID, expectedRevision uint64, value []byte, rank storage.Rank, due storage.Due) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Update"); err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := storage.ValidateOrderedID(id); err != nil {
		return storage.OrderedRecord{}, err
	}
	for attempt := 0; attempt < pginternal.Attempts(); attempt++ {
		record, err := s.updateOnce(ctx, id, expectedRevision, value, rank, due)
		if err == nil {
			return record, nil
		}
		var commitErr *commitOutcomeError
		if errors.As(err, &commitErr) {
			return s.resolveMutation(storage.OrderedUpdateOperation, id, record, commitErr.cause)
		}
		if isContractError(err) {
			return storage.OrderedRecord{}, err
		}
		if pginternal.Retryable(err) && ctx.Err() == nil && attempt+1 < pginternal.Attempts() {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return storage.OrderedRecord{}, ctxErr
		}
		return storage.OrderedRecord{}, failure(ctx, "ordered update")
	}
	panic("unreachable")
}

func (s *Store) updateOnce(ctx context.Context, id storage.OrderedID, expectedRevision uint64, value []byte, rank storage.Rank, due storage.Due) (storage.OrderedRecord, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return storage.OrderedRecord{}, err
	}
	current, err := s.getFrom(ctx, tx, id, " FOR UPDATE")
	if err == pgx.ErrNoRows {
		return storage.OrderedRecord{}, &storage.OrderedRecordNotFoundError{ID: id}
	}
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	if current.Deleted {
		return storage.OrderedRecord{}, &storage.OrderedDeletedError{ID: id}
	}
	if err := storage.ValidateOrderedValue(value); err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := storage.ValidateDue(due); err != nil {
		return storage.OrderedRecord{}, err
	}
	if current.Revision != expectedRevision {
		return storage.OrderedRecord{}, &storage.OrderedRevisionConflictError{ID: id, ExpectedRevision: expectedRevision, ActualRevision: current.Revision}
	}
	if current.Revision == math.MaxUint64 {
		return storage.OrderedRecord{}, &storage.OrderedRevisionExhaustedError{ID: id, Revision: current.Revision}
	}
	storedValue, valueIsNil := databaseValue(value)
	next := current
	next.Revision++
	next.Value, next.Rank, next.Due = cloneValue(value), rank, due
	_, err = tx.Exec(ctx, "UPDATE "+s.recordsTable()+" SET revision = revision + 1, value = $4, value_is_nil = $5, ranked = $6, rank_value = $7, due_state = $8, due_at = $9 WHERE namespace = $1 AND ordering_scope = $2 AND stable_key = $3", id.Namespace, id.OrderingScope, string(id.StableKey), storedValue, valueIsNil, rank.Ranked, rank.Value, int16(due.State), due.UnixMillis)
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := s.commit(ctx, tx); err != nil {
		if pginternal.Retryable(err) {
			return storage.OrderedRecord{}, err
		}
		return next, &commitOutcomeError{cause: safeCause(ctx, "ordered update commit")}
	}
	return next, nil
}

func (s *Store) Delete(ctx context.Context, id storage.OrderedID, expectedRevision uint64) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Delete"); err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := storage.ValidateOrderedID(id); err != nil {
		return storage.OrderedRecord{}, err
	}
	for attempt := 0; attempt < pginternal.Attempts(); attempt++ {
		record, err := s.deleteOnce(ctx, id, expectedRevision)
		if err == nil {
			return record, nil
		}
		var commitErr *commitOutcomeError
		if errors.As(err, &commitErr) {
			return s.resolveMutation(storage.OrderedDeleteOperation, id, record, commitErr.cause)
		}
		if isContractError(err) {
			return storage.OrderedRecord{}, err
		}
		if pginternal.Retryable(err) && ctx.Err() == nil && attempt+1 < pginternal.Attempts() {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return storage.OrderedRecord{}, ctxErr
		}
		return storage.OrderedRecord{}, failure(ctx, "ordered delete")
	}
	panic("unreachable")
}

func (s *Store) deleteOnce(ctx context.Context, id storage.OrderedID, expectedRevision uint64) (storage.OrderedRecord, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return storage.OrderedRecord{}, err
	}
	current, err := s.getFrom(ctx, tx, id, " FOR UPDATE")
	if err == pgx.ErrNoRows {
		return storage.OrderedRecord{}, &storage.OrderedRecordNotFoundError{ID: id}
	}
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	if current.Deleted {
		return current, nil
	}
	if current.Revision != expectedRevision {
		return storage.OrderedRecord{}, &storage.OrderedRevisionConflictError{ID: id, ExpectedRevision: expectedRevision, ActualRevision: current.Revision}
	}
	if current.Revision == math.MaxUint64 {
		return storage.OrderedRecord{}, &storage.OrderedRevisionExhaustedError{ID: id, Revision: current.Revision}
	}
	next := current
	next.Revision, next.Deleted, next.Rank, next.Due = next.Revision+1, true, storage.Rank{}, storage.Due{State: storage.NotDue}
	_, err = tx.Exec(ctx, "UPDATE "+s.recordsTable()+" SET revision = revision + 1, ranked = false, rank_value = 0, due_state = 0, due_at = 0, deleted = true WHERE namespace = $1 AND ordering_scope = $2 AND stable_key = $3", id.Namespace, id.OrderingScope, string(id.StableKey))
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	if err := s.commit(ctx, tx); err != nil {
		if pginternal.Retryable(err) {
			return storage.OrderedRecord{}, err
		}
		return next, &commitOutcomeError{cause: safeCause(ctx, "ordered delete commit")}
	}
	return next, nil
}

func (s *Store) ListOrdered(ctx context.Context, namespace, orderingScope string, afterOrder uint64, limit int) (storage.OrderedPage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListOrdered"); err != nil {
		return storage.OrderedPage{}, err
	}
	if err := storage.ValidateName(namespace); err != nil {
		return storage.OrderedPage{}, err
	}
	if err := storage.ValidateName(orderingScope); err != nil {
		return storage.OrderedPage{}, err
	}
	if err := storage.ValidateOrderedLimit(limit); err != nil {
		return storage.OrderedPage{}, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+recordColumns+" FROM "+s.recordsTable()+" WHERE namespace = $1 AND ordering_scope = $2 AND order_id > $3::numeric ORDER BY order_id ASC, stable_key ASC LIMIT $4", namespace, orderingScope, strconv.FormatUint(afterOrder, 10), limit)
	if err != nil {
		return storage.OrderedPage{}, failure(ctx, "ordered list order")
	}
	records, err := scanRecords(rows)
	if err != nil {
		return storage.OrderedPage{}, failure(ctx, "ordered list order")
	}
	page := storage.OrderedPage{Records: records}
	if len(records) > 0 {
		page.NextAfterOrder = records[len(records)-1].Order
	}
	return page, nil
}

func (s *Store) ListRanked(ctx context.Context, namespace, rankingScope string, after storage.RankedCursor, limit int) (storage.RankedPage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListRanked"); err != nil {
		return storage.RankedPage{}, err
	}
	if err := storage.ValidateName(namespace); err != nil {
		return storage.RankedPage{}, err
	}
	if err := storage.ValidateName(rankingScope); err != nil {
		return storage.RankedPage{}, err
	}
	if err := storage.ValidateOrderedLimit(limit); err != nil {
		return storage.RankedPage{}, err
	}
	position, hasPosition, err := decodeRankedCursor(after, namespace, rankingScope)
	if err != nil {
		return storage.RankedPage{}, err
	}
	query, args := "SELECT "+recordColumns+" FROM "+s.recordsTable()+" WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted", []any{namespace, rankingScope}
	if hasPosition {
		query += " AND (rank_value, stable_key, ordering_scope) < ($3, $4, $5)"
		args = append(args, position.rank, string(position.stableKey), position.orderingScope)
	}
	query += " ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return storage.RankedPage{}, failure(ctx, "ordered list ranked")
	}
	records, err := scanRecords(rows)
	if err != nil {
		return storage.RankedPage{}, failure(ctx, "ordered list ranked")
	}
	page := storage.RankedPage{}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor = encodeRankedCursor(namespace, rankingScope, last)
	}
	page.Records = records
	return page, nil
}

func (s *Store) ListDue(ctx context.Context, namespace string, dueAtOrBefore int64, after storage.DueCursor, limit int) (storage.DuePage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListDue"); err != nil {
		return storage.DuePage{}, err
	}
	if err := storage.ValidateName(namespace); err != nil {
		return storage.DuePage{}, err
	}
	if err := storage.ValidateOrderedLimit(limit); err != nil {
		return storage.DuePage{}, err
	}
	position, hasPosition, err := decodeDueCursor(after, namespace, dueAtOrBefore)
	if err != nil {
		return storage.DuePage{}, err
	}
	query, args := "SELECT "+recordColumns+" FROM "+s.recordsTable()+" WHERE namespace = $1 AND due_state = 1 AND NOT deleted AND due_at <= $2", []any{namespace, dueAtOrBefore}
	if hasPosition {
		query += " AND (due_at, stable_key, ordering_scope) > ($3, $4, $5)"
		args = append(args, position.dueAt, string(position.stableKey), position.orderingScope)
	}
	query += " ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return storage.DuePage{}, failure(ctx, "ordered list due")
	}
	records, err := scanRecords(rows)
	if err != nil {
		return storage.DuePage{}, failure(ctx, "ordered list due")
	}
	page := storage.DuePage{}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor = encodeDueCursor(namespace, dueAtOrBefore, last)
	}
	page.Records = records
	return page, nil
}

const recordColumns = "namespace, ordering_scope, stable_key, ranking_scope, revision::text, order_id::text, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted"

type rowScanner interface{ Scan(...any) error }
type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) get(ctx context.Context, id storage.OrderedID) (storage.OrderedRecord, error) {
	return s.getFrom(ctx, s.pool, id, "")
}
func (s *Store) getFrom(ctx context.Context, queryer queryRower, id storage.OrderedID, suffix string) (storage.OrderedRecord, error) {
	return scanRecord(queryer.QueryRow(ctx, "SELECT "+recordColumns+" FROM "+s.recordsTable()+" WHERE namespace = $1 AND ordering_scope = $2 AND stable_key = $3"+suffix, id.Namespace, id.OrderingScope, string(id.StableKey)))
}

func scanRecord(row rowScanner) (storage.OrderedRecord, error) {
	var record storage.OrderedRecord
	var revisionText, orderText, stableKey string
	var valueIsNil bool
	var dueState int16
	err := row.Scan(&record.ID.Namespace, &record.ID.OrderingScope, &stableKey, &record.RankingScope, &revisionText, &orderText, &record.Value, &valueIsNil, &record.Rank.Ranked, &record.Rank.Value, &dueState, &record.Due.UnixMillis, &record.Deleted)
	if err != nil {
		return storage.OrderedRecord{}, err
	}
	record.ID.StableKey = storage.StableKey(stableKey)
	if record.Revision, err = parseUint(revisionText); err != nil {
		return storage.OrderedRecord{}, err
	}
	if record.Order, err = parseUint(orderText); err != nil {
		return storage.OrderedRecord{}, err
	}
	if dueState < 0 || dueState > 255 {
		return storage.OrderedRecord{}, pginternal.RedactedError("ordered due-state decode")
	}
	record.Due.State = storage.DueState(dueState)
	if valueIsNil {
		record.Value = nil
	} else {
		record.Value = bytes.Clone(record.Value)
	}
	if err := storage.ValidateOrderedRecord(record); err != nil {
		return storage.OrderedRecord{}, err
	}
	return record, nil
}

func scanRecords(rows pgx.Rows) ([]storage.OrderedRecord, error) {
	defer rows.Close()
	records := make([]storage.OrderedRecord, 0)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func parseUint(text string) (uint64, error) {
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != text {
		return 0, pginternal.RedactedError("ordered integer decode")
	}
	return value, nil
}
func validateCandidate(scope string, value []byte, due storage.Due) error {
	if err := storage.ValidateName(scope); err != nil {
		return err
	}
	if err := storage.ValidateOrderedValue(value); err != nil {
		return err
	}
	return storage.ValidateDue(due)
}
func cloneValue(value []byte) []byte {
	if value == nil {
		return nil
	}
	return bytes.Clone(value)
}
func databaseValue(value []byte) ([]byte, bool) {
	if value == nil {
		return []byte{}, true
	}
	return value, false
}
func (s *Store) scopesTable() string {
	return pginternal.Qualified(s.schema, s.tablePrefix+orderedScopesSuffix)
}
func (s *Store) recordsTable() string {
	return pginternal.Qualified(s.schema, s.tablePrefix+orderedRecordsSuffix)
}

type commitOutcomeError struct{ cause error }

func (*commitOutcomeError) Error() string { return "ordered commit outcome unknown" }
func (s *Store) resolveCreate(id storage.OrderedID, want storage.OrderedRecord, cause error) (storage.OrderedRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	got, err := s.get(ctx, id)
	if err == nil && recordsEqual(got, want) {
		return got, true, nil
	}
	return storage.OrderedRecord{}, false, &storage.OrderedAmbiguousError{Operation: storage.OrderedCreateOperation, ID: id, Cause: cause}
}
func (s *Store) resolveMutation(operation storage.OrderedOperation, id storage.OrderedID, want storage.OrderedRecord, cause error) (storage.OrderedRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	got, err := s.get(ctx, id)
	if err == nil && mutationMatchesExactPostState(got, want) {
		return got, nil
	}
	return storage.OrderedRecord{}, &storage.OrderedAmbiguousError{Operation: operation, ID: id, Cause: cause}
}
func mutationMatchesExactPostState(got, want storage.OrderedRecord) bool {
	return recordsEqual(got, want)
}
func recordsEqual(a, b storage.OrderedRecord) bool {
	return a.ID == b.ID && a.RankingScope == b.RankingScope && a.Revision == b.Revision && a.Order == b.Order && a.Due == b.Due && a.Rank == b.Rank && bytes.Equal(a.Value, b.Value) && (a.Value == nil) == (b.Value == nil) && a.Deleted == b.Deleted
}
func isContractError(err error) bool {
	var a *storage.OrderedRecordNotFoundError
	var b *storage.OrderedDeletedError
	var c *storage.OrderedRevisionConflictError
	var d *storage.OrderedRevisionExhaustedError
	var e *orderExhaustedError
	var f *storage.InvalidNameError
	var g *storage.InvalidDueError
	var h *storage.OrderedValueTooLargeError
	return errors.As(err, &a) || errors.As(err, &b) || errors.As(err, &c) || errors.As(err, &d) || errors.As(err, &e) || errors.As(err, &f) || errors.As(err, &g) || errors.As(err, &h)
}

type orderExhaustedError struct{}

func (*orderExhaustedError) Error() string { return "pgstore: ordered acceptance order exhausted" }
func failure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}
func safeCause(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

type cursorEnvelope struct {
	Version       int    `json:"v"`
	Kind          string `json:"k"`
	Namespace     string `json:"n"`
	RankingScope  string `json:"s,omitempty"`
	DueBound      *int64 `json:"b,omitempty"`
	Position      int64  `json:"p"`
	StableKey     string `json:"x"`
	OrderingScope string `json:"o"`
}
type rankedPosition struct {
	rank          int64
	stableKey     storage.StableKey
	orderingScope string
}
type duePosition struct {
	dueAt         int64
	stableKey     storage.StableKey
	orderingScope string
}

func encodeRankedCursor(namespace, scope string, record storage.OrderedRecord) storage.RankedCursor {
	return storage.RankedCursor(encodeCursor(cursorEnvelope{Version: cursorVersion, Kind: string(storage.RankedCursorKind), Namespace: namespace, RankingScope: scope, Position: record.Rank.Value, StableKey: string(record.ID.StableKey), OrderingScope: record.ID.OrderingScope}))
}
func encodeDueCursor(namespace string, bound int64, record storage.OrderedRecord) storage.DueCursor {
	return storage.DueCursor(encodeCursor(cursorEnvelope{Version: cursorVersion, Kind: string(storage.DueCursorKind), Namespace: namespace, DueBound: &bound, Position: record.Due.UnixMillis, StableKey: string(record.ID.StableKey), OrderingScope: record.ID.OrderingScope}))
}
func encodeCursor(envelope cursorEnvelope) string {
	raw, err := json.Marshal(envelope)
	if err != nil {
		panic("ordered cursor scalar marshal failed")
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeRankedCursor(cursor storage.RankedCursor, namespace, scope string) (rankedPosition, bool, error) {
	if cursor == "" {
		return rankedPosition{}, false, nil
	}
	envelope, err := decodeCursor(string(cursor), storage.RankedCursorKind)
	if err != nil {
		return rankedPosition{}, false, err
	}
	if envelope.RankingScope == "" || envelope.DueBound != nil {
		return rankedPosition{}, false, cursorError(storage.RankedCursorKind, string(cursor), storage.OrderedCursorMalformed)
	}
	if envelope.Namespace != namespace || envelope.RankingScope != scope {
		return rankedPosition{}, false, cursorError(storage.RankedCursorKind, string(cursor), storage.OrderedCursorQueryMismatch)
	}
	return rankedPosition{envelope.Position, storage.StableKey(envelope.StableKey), envelope.OrderingScope}, true, nil
}
func decodeDueCursor(cursor storage.DueCursor, namespace string, bound int64) (duePosition, bool, error) {
	if cursor == "" {
		return duePosition{}, false, nil
	}
	envelope, err := decodeCursor(string(cursor), storage.DueCursorKind)
	if err != nil {
		return duePosition{}, false, err
	}
	if envelope.RankingScope != "" || envelope.DueBound == nil {
		return duePosition{}, false, cursorError(storage.DueCursorKind, string(cursor), storage.OrderedCursorMalformed)
	}
	if envelope.Namespace != namespace || *envelope.DueBound != bound {
		return duePosition{}, false, cursorError(storage.DueCursorKind, string(cursor), storage.OrderedCursorQueryMismatch)
	}
	return duePosition{envelope.Position, storage.StableKey(envelope.StableKey), envelope.OrderingScope}, true, nil
}
func decodeCursor(cursor string, expected storage.OrderedCursorKind) (cursorEnvelope, error) {
	if len(cursor) > maxCursorEncoded {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(raw) == 0 || len(raw) > maxCursorDecoded {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	var header struct {
		Version *int    `json:"v"`
		Kind    *string `json:"k"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.Version == nil || header.Kind == nil {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	if *header.Version != cursorVersion {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorUnknownVersion)
	}
	if *header.Kind != string(expected) {
		if *header.Kind == string(storage.RankedCursorKind) || *header.Kind == string(storage.DueCursorKind) {
			return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorWrongKind)
		}
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope cursorEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	if decoder.Decode(&struct{}{}) == nil {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, raw) {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	if storage.ValidateName(envelope.Namespace) != nil || storage.ValidateStableKey(storage.StableKey(envelope.StableKey)) != nil || storage.ValidateName(envelope.OrderingScope) != nil {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	if envelope.RankingScope != "" && storage.ValidateName(envelope.RankingScope) != nil {
		return cursorEnvelope{}, cursorError(expected, cursor, storage.OrderedCursorMalformed)
	}
	return envelope, nil
}
func cursorError(kind storage.OrderedCursorKind, cursor string, rule storage.OrderedCursorRule) error {
	return storage.NewInvalidOrderedCursorError(kind, cursor, rule)
}

var _ storage.OrderedIndex = (*Store)(nil)
