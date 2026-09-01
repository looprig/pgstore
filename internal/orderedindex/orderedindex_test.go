package orderedindex

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/storage"
)

type outOfRangeDueRow struct{}

func (outOfRangeDueRow) Scan(dest ...any) error {
	*dest[0].(*string) = "sessions"
	*dest[1].(*string) = "acceptance"
	*dest[2].(*[]byte) = []byte("key")
	*dest[3].(*string) = "workers"
	*dest[4].(*string) = "1"
	*dest[5].(*string) = "1"
	*dest[6].(*[]byte) = []byte("value")
	*dest[7].(*bool) = false
	*dest[8].(*bool) = false
	*dest[9].(*int64) = 0
	*dest[10].(*int16) = 256
	*dest[11].(*int64) = 0
	*dest[12].(*bool) = false
	return nil
}

func TestScanRecordRejectsDueStateBeforeNarrowing(t *testing.T) {
	if _, err := scanRecord(outOfRangeDueRow{}); err == nil {
		t.Fatal("scanRecord accepted due_state 256, which would wrap to NotDue")
	}
}

func TestCursorDecoderRejectsOversizeAndNoncanonicalTokens(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{name: "encoded ceiling", token: strings.Repeat("A", maxCursorEncoded+1)},
		{name: "decoded ceiling", token: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", maxCursorDecoded+1)))},
		{name: "noncanonical field order", token: base64.RawURLEncoding.EncodeToString([]byte(`{"k":"ranked","v":1,"n":"sessions","s":"workers","p":1,"x":"key","o":"acceptance"}`))},
		{name: "unknown field", token: base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"k":"ranked","n":"sessions","s":"workers","p":1,"x":"key","o":"acceptance","extra":1}`))},
		{name: "missing version and kind", token: base64.RawURLEncoding.EncodeToString([]byte(`{}`))},
		{name: "null envelope", token: base64.RawURLEncoding.EncodeToString([]byte(`null`))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := decodeRankedCursor(storage.RankedCursor(test.token), "sessions", "workers")
			var invalid *storage.InvalidOrderedCursorError
			if !errors.As(err, &invalid) || invalid.Kind != storage.RankedCursorKind || invalid.Rule != storage.OrderedCursorMalformed {
				t.Fatalf("decode = %T %v, want ranked malformed cursor", err, err)
			}
			if strings.Contains(err.Error(), test.token) {
				t.Fatal("cursor error disclosed opaque token")
			}
		})
	}
}

func TestCursorDecoderClassifiesVersionKindAndQueryBeforeUse(t *testing.T) {
	record := storage.OrderedRecord{
		ID:           storage.OrderedID{Namespace: "sessions", OrderingScope: "acceptance", StableKey: "key"},
		RankingScope: "workers", Rank: storage.Rank{Ranked: true, Value: 7}, Due: storage.Due{State: storage.DueAt, UnixMillis: 8},
	}
	ranked := encodeRankedCursor("sessions", "workers", record)
	due := encodeDueCursor("sessions", 10, record)
	assertCursorRule(t, func() error {
		_, _, err := decodeRankedCursor(storage.RankedCursor(due), "sessions", "workers")
		return err
	}, storage.RankedCursorKind, storage.OrderedCursorWrongKind)
	assertCursorRule(t, func() error { _, _, err := decodeDueCursor(storage.DueCursor(ranked), "sessions", 10); return err }, storage.DueCursorKind, storage.OrderedCursorWrongKind)
	assertCursorRule(t, func() error { _, _, err := decodeRankedCursor(ranked, "sessions", "other"); return err }, storage.RankedCursorKind, storage.OrderedCursorQueryMismatch)
	assertCursorRule(t, func() error { _, _, err := decodeDueCursor(due, "sessions", 11); return err }, storage.DueCursorKind, storage.OrderedCursorQueryMismatch)
	unknown := base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"k":"ranked"}`))
	assertCursorRule(t, func() error {
		_, _, err := decodeRankedCursor(storage.RankedCursor(unknown), "sessions", "workers")
		return err
	}, storage.RankedCursorKind, storage.OrderedCursorUnknownVersion)
}

func assertCursorRule(t *testing.T, call func() error, kind storage.OrderedCursorKind, rule storage.OrderedCursorRule) {
	t.Helper()
	err := call()
	var invalid *storage.InvalidOrderedCursorError
	if !errors.As(err, &invalid) || invalid.Kind != kind || invalid.Rule != rule {
		t.Fatalf("decode = %T %v, want %s/%s", err, err, kind, rule)
	}
}
