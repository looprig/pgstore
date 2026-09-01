package pgstore

import (
	"os"
	"strings"
	"testing"
)

func TestKVKeysPinsBytewiseCollation(t *testing.T) {
	source, err := os.ReadFile("internal/kv/kv.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `ORDER BY key COLLATE \"C\"`) {
		t.Fatal(`KV.Keys query does not pin ORDER BY key COLLATE "C"`)
	}
}
