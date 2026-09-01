// Package orderedquery owns the SQL statements used by OrderedIndex page reads.
package orderedquery

import "strconv"

// RecordColumns is the scan order shared by direct and page reads.
const RecordColumns = "namespace, ordering_scope, stable_key, ranking_scope, revision::text, order_id::text, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted"

// Statement is a parameterized page query and its ordered bound arguments.
type Statement struct {
	SQL  string
	Args []any
}

// RankedPosition is the complete tuple following a ranked cursor.
type RankedPosition struct {
	Rank          int64
	StableKey     []byte
	OrderingScope string
}

// DuePosition is the complete tuple following a due cursor.
type DuePosition struct {
	DueAt         int64
	StableKey     []byte
	OrderingScope string
}

// Ordered builds the immutable-order page statement.
func Ordered(table, namespace, orderingScope string, afterOrder uint64, limit int) Statement {
	return Statement{
		SQL:  "SELECT " + RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 AND order_id > $3::numeric ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $4",
		Args: []any{namespace, orderingScope, strconv.FormatUint(afterOrder, 10), limit},
	}
}

// Ranked builds a first page when position is nil and a keyset page otherwise.
func Ranked(table, namespace, rankingScope string, position *RankedPosition, limit int) Statement {
	query := "SELECT " + RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted"
	args := []any{namespace, rankingScope}
	if position != nil {
		query += " AND (rank_value, stable_key, ordering_scope) < ($3, $4, $5)"
		args = append(args, position.Rank, position.StableKey, position.OrderingScope)
	}
	query += " ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT $" + strconv.Itoa(len(args)+1)
	return Statement{SQL: query, Args: append(args, limit+1)}
}

// Due builds a first page when position is nil and a keyset page otherwise.
func Due(table, namespace string, dueAtOrBefore int64, position *DuePosition, limit int) Statement {
	query := "SELECT " + RecordColumns + " FROM " + table + " WHERE namespace = $1 AND due_state = 1 AND NOT deleted AND due_at <= $2"
	args := []any{namespace, dueAtOrBefore}
	if position != nil {
		query += " AND (due_at, stable_key, ordering_scope) > ($3, $4, $5)"
		args = append(args, position.DueAt, position.StableKey, position.OrderingScope)
	}
	query += " ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $" + strconv.Itoa(len(args)+1)
	return Statement{SQL: query, Args: append(args, limit+1)}
}
