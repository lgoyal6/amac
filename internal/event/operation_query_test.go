// Checks the operation receipt lookup used by log.go.
// The SQLite planner must use the partial operation index as operation history grows.
// Querying an exact operation must not scan unrelated operation records.
package event

import (
	"context"
	"strings"
	"testing"
)

func TestOperationReceiptLookupUsesPartialIndex(t *testing.T) {
	log, err := Open(t.TempDir()+"/events.db", Full)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	rows, err := log.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+operationEventsSQL,
		"op-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "USING INDEX idx_events_operation") {
		t.Fatalf("operation lookup misses its exact-key index: %v", plan)
	}
}
