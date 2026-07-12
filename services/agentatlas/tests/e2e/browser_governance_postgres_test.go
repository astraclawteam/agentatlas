package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	governancemodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBrowserGovernancePostgres(t *testing.T) {
	dsn := os.Getenv("ATLAS_TASK8_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ATLAS_TASK8_POSTGRES_DSN not set (dedicated database required)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	suffix := time.Now().UTC().Format("150405.000000000")
	ent, workflowID := "ent-browser-"+suffix, "wf-browser-"+suffix
	if _, err = pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Browser E2E')`, ent); err != nil {
		t.Fatal(err)
	}
	def := workflow.Definition{WorkflowID: workflowID, Kind: sdkworkflow.KindSOP, RiskLevel: sdkworkflow.RiskHigh, Nodes: []sdkworkflow.Node{{ID: "input", Type: sdkworkflow.NodeInputManual}, {ID: "confirm", Type: sdkworkflow.NodeHumanConfirm, RequiresConfirmation: true}}, Edges: []sdkworkflow.Edge{{From: "input", To: "confirm"}}}
	raw, _ := json.Marshal(def)
	if _, err = pool.Exec(ctx, `INSERT INTO workflows(id,enterprise_id,name,kind,created_by,draft) VALUES($1,$2,'Governed SOP','sop','editor',$3)`, workflowID, ent, raw); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	protector, _ := browsersession.NewProtector(key)
	sessionStore, _ := browsersession.NewPostgresStore(pool, protector, time.Now)
	attempt, err := sessionStore.CreateLoginAttempt(ctx, browsersession.LoginAttemptInput{State: "state-1234567890123456", Nonce: "nonce-1234567890123456", PKCEVerifier: "verifier-1234567890123456789012345678901234567890123", ReturnTo: "/changes/test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionStore.ConsumeLoginAttempt(ctx, attempt.State); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionStore.ConsumeLoginAttempt(ctx, attempt.State); err == nil {
		t.Fatal("login state reused")
	}
	token := "atlas-session-token-" + suffix
	identity := browsersession.Identity{EnterpriseID: ent, UserID: "editor", DisplayName: "Editor", OrgVersion: 7, OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit", "publish_low_risk"}}
	if err = sessionStore.CreateSession(ctx, token, identity, "upstream-access-token-123456", 8*time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	session, err := sessionStore.GetSession(ctx, token)
	if err != nil || session.UpstreamAccessToken != "upstream-access-token-123456" {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	var rawCredentialCount int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_sessions WHERE position('upstream-access-token' in encode(upstream_access_token_ciphertext,'escape'))>0`).Scan(&rawCredentialCount); err != nil || rawCredentialCount != 0 {
		t.Fatalf("raw upstream token persisted count=%d err=%v", rawCredentialCount, err)
	}
	store, _ := governance.NewPostgresStore(pool, time.Now)
	audits := &governance.MemoryAuditAppender{}
	svc := governance.NewService(store, governance.StaticRouteResolver{ReviewerUserID: "manager", OrgPath: []string{"team", "department"}}, audits, nil, time.Now)
	editor := governance.Actor{EnterpriseID: ent, UserID: "editor", OrgUnitIDs: []string{"team"}, Permissions: []string{"workflow_edit"}}
	draft, err := svc.CreateDraft(ctx, editor, governance.SuggestionInput{OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, ProposedContent: raw})
	if err != nil {
		t.Fatal(err)
	}
	route, err := svc.Submit(ctx, editor, draft.ChangeID)
	if err != nil || route.ReviewerUserID != "manager" {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	reviewer := governance.Actor{EnterpriseID: ent, UserID: "manager", OrgUnitIDs: []string{"department"}, Permissions: []string{"approve_high_risk"}}
	if err = svc.Decide(ctx, reviewer, draft.ChangeID, "postgres-review-decision-key-0001", governance.DecisionInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result governance.PublishedVersion
		err    error
	}
	outcomes := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := svc.Publish(ctx, editor, draft.ChangeID, "pg-publish-key-123456")
			outcomes <- outcome{result, err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	results := []governance.PublishedVersion{}
	for got := range outcomes {
		if got.err != nil {
			t.Fatalf("concurrent publish: %v", got.err)
		}
		results = append(results, got.result)
	}
	if len(results) != 2 || results[0] != results[1] || audits.Count() != 1 {
		t.Fatalf("results=%+v audits=%d", results, audits.Count())
	}
	first := results[0]
	second, err := svc.Publish(ctx, editor, draft.ChangeID, "pg-publish-key-123456")
	if err != nil || first != second {
		t.Fatalf("replay first=%+v second=%+v err=%v", first, second, err)
	}
	stale, err := svc.CreateDraft(ctx, editor, governance.SuggestionInput{OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, BaseVersion: 0, ProposedContent: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, stale.ChangeID); err != nil {
		t.Fatal(err)
	}
	if err = svc.Decide(ctx, reviewer, stale.ChangeID, "postgres-review-decision-key-0002", governance.DecisionInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Publish(ctx, editor, stale.ChangeID, "pg-stale-base-key-1234"); !errors.Is(err, governance.ErrConflict) {
		t.Fatalf("stale base published: %v", err)
	}

	tracingDraft, err := svc.CreateDraft(ctx, editor, governance.SuggestionInput{OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, BaseVersion: 1, ProposedContent: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, tracingDraft.ChangeID); err != nil {
		t.Fatal(err)
	}
	decisionStart := make(chan struct{})
	decisionResults := make(chan error, 2)
	for i, decision := range []string{"approve", "reject"} {
		go func(i int, decision string) {
			<-decisionStart
			decisionResults <- svc.Decide(ctx, reviewer, tracingDraft.ChangeID, []string{"pg-race-approve-key-0001", "pg-race-reject-key-0001"}[i], governance.DecisionInput{Decision: decision})
		}(i, decision)
	}
	close(decisionStart)
	decisionErrs := []error{<-decisionResults, <-decisionResults}
	if (decisionErrs[0] == nil) == (decisionErrs[1] == nil) || (decisionErrs[0] != nil && !errors.Is(decisionErrs[0], governance.ErrConflict)) || (decisionErrs[1] != nil && !errors.Is(decisionErrs[1], governance.ErrConflict)) {
		t.Fatalf("concurrent decision results=%v, want one success and one conflict", decisionErrs)
	}
	var decisionOperations, decisionAudits int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM change_decision_operations WHERE enterprise_id=$1 AND change_id=$2`, ent, tracingDraft.ChangeID).Scan(&decisionOperations); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM change_decision_audits WHERE enterprise_id=$1 AND change_id=$2`, ent, tracingDraft.ChangeID).Scan(&decisionAudits); err != nil {
		t.Fatal(err)
	}
	if decisionOperations != 1 || decisionAudits != 1 {
		t.Fatalf("decision operations=%d audits=%d, want 1/1", decisionOperations, decisionAudits)
	}

	retryDraft, err := svc.CreateDraft(ctx, editor, governance.SuggestionInput{OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, BaseVersion: 1, ProposedContent: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Submit(ctx, editor, retryDraft.ChangeID); err != nil {
		t.Fatal(err)
	}
	lostStore := &postgresLostDecisionResponseStore{Store: store}
	retrySvc := governance.NewService(lostStore, governance.StaticRouteResolver{ReviewerUserID: "manager", OrgPath: []string{"team", "department"}}, audits, nil, time.Now)
	retryKey := "pg-response-loss-key-0001"
	if err = retrySvc.Decide(ctx, reviewer, retryDraft.ChangeID, retryKey, governance.DecisionInput{Decision: "approve"}); err == nil {
		t.Fatal("first decision response was not lost")
	}
	if err = retrySvc.Decide(ctx, reviewer, retryDraft.ChangeID, retryKey, governance.DecisionInput{Decision: "approve"}); err != nil {
		t.Fatalf("decision retry: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM change_decision_audits WHERE enterprise_id=$1 AND change_id=$2`, ent, retryDraft.ChangeID).Scan(&decisionAudits); err != nil {
		t.Fatal(err)
	}
	if decisionAudits != 1 {
		t.Fatalf("response-loss decision audits=%d, want 1", decisionAudits)
	}

	staleSubmitDraft, err := svc.CreateDraft(ctx, editor, governance.SuggestionInput{OrgUnitID: "team", ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, Action: governancemodel.ActionPublish, BaseVersion: 1, ProposedContent: raw})
	if err != nil {
		t.Fatal(err)
	}
	staleSubmit, err := store.Get(ctx, ent, staleSubmitDraft.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	staleSubmit.Draft.State = governancemodel.ChangeSubmitted
	staleSubmit.Assessment = governancemodel.RiskAssessment{RiskLevel: governancemodel.RiskHigh, RiskReasons: []string{"workflow"}}
	staleSubmit.Route = governancemodel.ReviewRoute{ChangeID: staleSubmitDraft.ChangeID, ResourceType: governancemodel.ResourceWorkflow, ResourceID: workflowID, RequesterUserID: "editor", ReviewerUserID: "manager", RiskLevel: governancemodel.RiskHigh, Mode: governancemodel.ReviewUpward, State: governancemodel.RoutePending, OrgPath: []string{"team", "department"}}
	if _, err = svc.Submit(ctx, editor, staleSubmitDraft.ChangeID); err != nil {
		t.Fatal(err)
	}
	if err = svc.Decide(ctx, reviewer, staleSubmitDraft.ChangeID, "pg-stale-submit-decision-001", governance.DecisionInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveReview(ctx, ent, staleSubmit); !errors.Is(err, governance.ErrConflict) {
		t.Fatalf("stale postgres submit=%v, want conflict", err)
	}
	staleCurrent, err := store.Get(ctx, ent, staleSubmitDraft.ChangeID)
	if err != nil || staleCurrent.Draft.State != governancemodel.ChangeApproved || staleCurrent.Route.State != governancemodel.RouteApproved {
		t.Fatalf("postgres final decision resurrected: %+v err=%v", staleCurrent, err)
	}
	var versions, pointers, operations int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow_versions WHERE workflow_id=$1`, workflowID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM published_resource_pointers WHERE enterprise_id=$1 AND resource_id=$2`, ent, workflowID).Scan(&pointers); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM publish_operations WHERE enterprise_id=$1 AND change_id=$2 AND status='succeeded'`, ent, draft.ChangeID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || pointers != 1 || operations != 1 {
		t.Fatalf("versions=%d pointers=%d operations=%d", versions, pointers, operations)
	}
}

type postgresLostDecisionResponseStore struct {
	governance.Store
	mu   sync.Mutex
	lost bool
}

func (s *postgresLostDecisionResponseStore) FinalizeDecision(ctx context.Context, ent, key string, actor governance.Actor, rec governance.Record, in governance.DecisionInput, auditRef string) error {
	if err := s.Store.FinalizeDecision(ctx, ent, key, actor, rec, in, auditRef); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lost {
		s.lost = true
		return errors.New("response lost after postgres commit")
	}
	return nil
}
