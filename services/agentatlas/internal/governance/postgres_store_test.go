package governance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresGovernedPublishFinalizesOneTransactionAfterAuditReceipt(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("postgres_store.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ToLower(string(raw))
	begin := strings.Index(src, "begin(ctx")
	audit := strings.Index(src, "audit_ref_id")
	version := strings.Index(src, "insert into change_versions")
	pointer := strings.Index(src, "insert into published_resource_pointers")
	complete := strings.Index(src, "status='succeeded'")
	commit := strings.LastIndex(src, "commit(ctx)")
	if begin < 0 || audit < begin || version < audit || pointer < version || complete < pointer || commit < complete {
		t.Fatalf("publish transaction order begin=%d audit=%d version=%d pointer=%d complete=%d commit=%d", begin, audit, version, pointer, complete, commit)
	}
}
