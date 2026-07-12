package dream

import (
	"context"
	"reflect"
	"testing"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

type lineageRecorder struct {
	rows []db.CreateDreamRunLineageParams
}

func (l *lineageRecorder) CreateDreamRunLineage(_ context.Context, p db.CreateDreamRunLineageParams) (db.DreamRunLineage, error) {
	l.rows = append(l.rows, p)
	return db.DreamRunLineage{RunID: p.RunID, ParentRunID: p.ParentRunID, Relation: p.Relation}, nil
}

func TestPersistDreamLineageIsExactSortedAndDeduplicated(t *testing.T) {
	recorder := &lineageRecorder{}
	inputs := []ResolvedInput{{ParentRunID: "child-b"}, {ParentRunID: ""}, {ParentRunID: "child-a"}, {ParentRunID: "child-b"}}
	if err := persistDreamLineage(context.Background(), recorder, "parent", inputs); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(recorder.rows))
	for _, row := range recorder.rows {
		if row.RunID != "parent" || row.Relation != "child_summary" {
			t.Fatalf("bad row: %+v", row)
		}
		got = append(got, row.ParentRunID)
	}
	if want := []string{"child-a", "child-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage=%v want=%v", got, want)
	}
}
