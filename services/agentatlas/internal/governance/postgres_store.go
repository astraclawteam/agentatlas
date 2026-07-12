package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewPostgresStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("governance postgres store requires pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresStore{pool: pool, now: now}, nil
}

func (s *PostgresStore) Create(ctx context.Context, r Record) (Record, error) {
	content := r.Content
	if len(content) == 0 {
		content, _ = json.Marshal(r.Draft.ProposedContent)
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO change_drafts(id,enterprise_id,org_unit_id,resource_type,resource_id,action,requester_user_id,origin,permission_mode,revision,state,base_version,proposed_content,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14) RETURNING created_at,updated_at`, r.Draft.ChangeID, r.Draft.EnterpriseID, r.Draft.OrgUnitID, r.Draft.ResourceType, r.Draft.ResourceID, r.Draft.Action, r.Draft.RequesterUserID, r.Draft.Origin, r.Draft.PermissionMode, r.Draft.Revision, r.Draft.State, r.Draft.BaseVersion, content, r.Draft.CreatedAt).Scan(&r.Draft.CreatedAt, &r.Draft.UpdatedAt)
	if err != nil {
		return Record{}, err
	}
	r.Content = clone(content)
	return r, nil
}

func (s *PostgresStore) Get(ctx context.Context, ent, id string) (Record, error) {
	var r Record
	var resourceType, action, origin, permission, state string
	err := s.pool.QueryRow(ctx, `SELECT id,enterprise_id,org_unit_id,resource_type,resource_id,action,requester_user_id,origin,permission_mode,revision,state,base_version,proposed_content,created_at,updated_at FROM change_drafts WHERE enterprise_id=$1 AND id=$2`, ent, id).Scan(&r.Draft.ChangeID, &r.Draft.EnterpriseID, &r.Draft.OrgUnitID, &resourceType, &r.Draft.ResourceID, &action, &r.Draft.RequesterUserID, &origin, &permission, &r.Draft.Revision, &state, &r.Draft.BaseVersion, &r.Content, &r.Draft.CreatedAt, &r.Draft.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	r.Draft.ResourceType = model.ResourceType(resourceType)
	r.Draft.Action = model.Action(action)
	r.Draft.Origin = model.ChangeOrigin(origin)
	r.Draft.PermissionMode = model.PermissionMode(permission)
	r.Draft.State = model.ChangeState(state)
	if json.Unmarshal(r.Content, &r.Draft.ProposedContent) != nil {
		return Record{}, ErrInvalidState
	}
	baseErr := s.pool.QueryRow(ctx, `SELECT version.content FROM published_resource_pointers AS pointer JOIN change_versions AS version ON version.enterprise_id=pointer.enterprise_id AND version.change_id=pointer.change_id AND version.version=pointer.change_version WHERE pointer.enterprise_id=$1 AND pointer.resource_type=$2 AND pointer.resource_id=$3`, ent, r.Draft.ResourceType, r.Draft.ResourceID).Scan(&r.BaseContent)
	if baseErr != nil && !errors.Is(baseErr, pgx.ErrNoRows) {
		return Record{}, baseErr
	}
	var reasons, path []byte
	var reviewer, queue *string
	var risk, mode, routeState, decision, decisionBy string
	reviewErr := s.pool.QueryRow(ctx, `SELECT risk_level,risk_reasons,review_mode,state,reviewer_user_id,org_path,queue,decision,COALESCE(reviewer_user_id,'') FROM change_reviews WHERE enterprise_id=$1 AND change_id=$2 AND change_revision=$3 ORDER BY created_at DESC LIMIT 1`, ent, id, r.Draft.Revision).Scan(&risk, &reasons, &mode, &routeState, &reviewer, &path, &queue, &decision, &decisionBy)
	if reviewErr == nil {
		r.Assessment.RiskLevel = model.RiskLevel(risk)
		_ = json.Unmarshal(reasons, &r.Assessment.RiskReasons)
		r.Route = model.ReviewRoute{ChangeID: id, ResourceType: r.Draft.ResourceType, ResourceID: r.Draft.ResourceID, RequesterUserID: r.Draft.RequesterUserID, RiskLevel: model.RiskLevel(risk), Mode: model.ReviewMode(mode), State: model.RouteState(routeState)}
		if reviewer != nil {
			r.Route.ReviewerUserID = *reviewer
		}
		if queue != nil {
			r.Route.Queue = *queue
		}
		_ = json.Unmarshal(path, &r.Route.OrgPath)
		r.Decision, r.DecisionBy = decision, decisionBy
	} else if !errors.Is(reviewErr, pgx.ErrNoRows) {
		return Record{}, reviewErr
	}
	return r, nil
}

func (s *PostgresStore) List(ctx context.Context, ent, org string, limit int) ([]Record, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM change_drafts WHERE enterprise_id=$1 AND org_unit_id=$2 ORDER BY updated_at DESC,id LIMIT $3`, ent, org, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			return nil, rows.Err()
		}
		ids = append(ids, id)
	}
	if len(ids) > limit {
		return nil, fmt.Errorf("governance bounded read exceeded %d", limit)
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		r, err := s.Get(ctx, ent, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Update(ctx context.Context, ent, id string, revision int32, content json.RawMessage) (Record, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE change_drafts SET proposed_content=$4,revision=revision+1,state='draft',updated_at=$5 WHERE enterprise_id=$1 AND id=$2 AND revision=$3 AND state IN ('draft','rejected')`, ent, id, revision, content, s.now().UTC())
	if err != nil {
		return Record{}, err
	}
	if tag.RowsAffected() == 0 {
		current, err := s.Get(ctx, ent, id)
		if err != nil {
			return Record{}, err
		}
		return Record{}, &ConflictError{CurrentRevision: current.Draft.Revision, Diff: makeDiff(current.Content, content)}
	}
	return s.Get(ctx, ent, id)
}

func (s *PostgresStore) SaveReview(ctx context.Context, ent string, r Record) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE change_drafts SET state=$4,updated_at=$5 WHERE enterprise_id=$1 AND id=$2 AND revision=$3`, ent, r.Draft.ChangeID, r.Draft.Revision, r.Draft.State, s.now().UTC())
	if err != nil || tag.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	reasons, _ := json.Marshal(r.Assessment.RiskReasons)
	path, _ := json.Marshal(r.Route.OrgPath)
	var reviewer, queue any
	if r.Route.ReviewerUserID != "" {
		reviewer = r.Route.ReviewerUserID
	}
	if r.Route.Queue != "" {
		queue = r.Route.Queue
	}
	tag, err = tx.Exec(ctx, `UPDATE change_reviews SET state=$5,decision=$6,comment=$7 WHERE enterprise_id=$1 AND change_id=$2 AND change_revision=$3 AND ((reviewer_user_id IS NULL AND $4::text IS NULL) OR reviewer_user_id=$4)`, ent, r.Draft.ChangeID, r.Draft.Revision, reviewer, r.Route.State, r.Decision, r.DecisionComment)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = tx.Exec(ctx, `INSERT INTO change_reviews(id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,risk_reasons,review_mode,state,org_path,queue,decision,comment) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, stableID("review", ent, r.Draft.ChangeID, fmt.Sprint(r.Draft.Revision)), ent, r.Draft.ChangeID, r.Draft.Revision, reviewer, r.Assessment.RiskLevel, reasons, r.Route.Mode, r.Route.State, path, queue, r.Decision, r.DecisionComment)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) BeginPublish(ctx context.Context, ent, idem, id string, revision int32, payload string) (PublishOperation, bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO publish_operations(id,enterprise_id,change_id,change_revision,idempotency_key,status,request_hash) VALUES($1,$2,$3,$4,$5,'pending',$6) ON CONFLICT(enterprise_id,idempotency_key) DO NOTHING`, stableID("pub", ent, idem), ent, id, revision, idem, payload)
	if err != nil {
		return PublishOperation{}, false, err
	}
	var op PublishOperation
	var status string
	var result []byte
	err = s.pool.QueryRow(ctx, `SELECT change_id,change_revision,COALESCE(request_hash,''),status,COALESCE(result,'null'::jsonb) FROM publish_operations WHERE enterprise_id=$1 AND idempotency_key=$2`, ent, idem).Scan(&op.ChangeID, &op.Revision, &op.PayloadHash, &status, &result)
	if err != nil {
		return PublishOperation{}, false, err
	}
	op.Complete = status == "succeeded"
	if op.Complete && json.Unmarshal(result, &op.Result) != nil {
		return PublishOperation{}, false, ErrInvalidState
	}
	return op, tag.RowsAffected() == 0, nil
}

func (s *PostgresStore) FinalizePublish(ctx context.Context, ent, idem string, actor Actor, rec Record, auditRef string) (PublishedVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublishedVersion{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ent+"|"+string(rec.Draft.ResourceType)+"|"+rec.Draft.ResourceID); err != nil {
		return PublishedVersion{}, err
	}
	var status, opChange string
	var opRevision int32
	var existingResult []byte
	if err = tx.QueryRow(ctx, `SELECT status,change_id,change_revision,COALESCE(result,'null'::jsonb) FROM publish_operations WHERE enterprise_id=$1 AND idempotency_key=$2 FOR UPDATE`, ent, idem).Scan(&status, &opChange, &opRevision, &existingResult); err != nil {
		return PublishedVersion{}, err
	}
	if opChange != rec.Draft.ChangeID || opRevision != rec.Draft.Revision || auditRef == "" {
		return PublishedVersion{}, ErrConflict
	}
	if status == "succeeded" {
		var replay PublishedVersion
		if json.Unmarshal(existingResult, &replay) != nil {
			return PublishedVersion{}, ErrInvalidState
		}
		return replay, nil
	}
	if status != "pending" {
		return PublishedVersion{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE publish_operations SET audit_ref_id=$3 WHERE enterprise_id=$1 AND idempotency_key=$2 AND audit_ref_id IS NULL`, ent, idem, auditRef); err != nil {
		return PublishedVersion{}, err
	}
	currentResourceVersion := int32(0)
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM workflow_versions WHERE workflow_id=$1`, rec.Draft.ResourceID).Scan(&currentResourceVersion); err != nil {
			return PublishedVersion{}, err
		}
	} else {
		versionErr := tx.QueryRow(ctx, `SELECT resource_version FROM published_resource_pointers WHERE enterprise_id=$1 AND resource_type=$2 AND resource_id=$3`, ent, rec.Draft.ResourceType, rec.Draft.ResourceID).Scan(&currentResourceVersion)
		if versionErr != nil && !errors.Is(versionErr, pgx.ErrNoRows) {
			return PublishedVersion{}, versionErr
		}
	}
	if currentResourceVersion != rec.Draft.BaseVersion {
		return PublishedVersion{}, &ConflictError{CurrentRevision: rec.Draft.Revision, Diff: makeDiff(rec.BaseContent, rec.Content)}
	}
	var changeVersion int32
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM change_versions WHERE enterprise_id=$1 AND change_id=$2`, ent, rec.Draft.ChangeID).Scan(&changeVersion); err != nil {
		return PublishedVersion{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO change_versions(id,enterprise_id,change_id,version,content,published_by) VALUES($1,$2,$3,$4,$5,$6)`, stableID("cv", ent, rec.Draft.ChangeID, fmt.Sprint(changeVersion)), ent, rec.Draft.ChangeID, changeVersion, rec.Content, actor.UserID); err != nil {
		return PublishedVersion{}, err
	}
	domainVersion := changeVersion
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		var wfEnterprise string
		if err = tx.QueryRow(ctx, `SELECT enterprise_id FROM workflows WHERE id=$1 FOR UPDATE`, rec.Draft.ResourceID).Scan(&wfEnterprise); err != nil || wfEnterprise != ent {
			if err == nil {
				err = ErrNotFound
			}
			return PublishedVersion{}, err
		}
		var def workflow.Definition
		if json.Unmarshal(rec.Content, &def) != nil {
			return PublishedVersion{}, ErrInvalidState
		}
		validator, verr := workflow.NewValidator()
		if verr != nil || validator.Validate(def) != nil {
			return PublishedVersion{}, ErrInvalidState
		}
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM workflow_versions WHERE workflow_id=$1`, rec.Draft.ResourceID).Scan(&domainVersion); err != nil {
			return PublishedVersion{}, err
		}
		def.WorkflowID = rec.Draft.ResourceID
		def.Version = int(domainVersion)
		definition, _ := json.Marshal(def)
		if _, err = tx.Exec(ctx, `UPDATE workflows SET draft=$3,draft_updated_at=now() WHERE id=$1 AND enterprise_id=$2`, rec.Draft.ResourceID, ent, definition); err != nil {
			return PublishedVersion{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workflow_versions(workflow_id,version,definition,risk_level,published_by) VALUES($1,$2,$3,$4,$5)`, rec.Draft.ResourceID, domainVersion, definition, def.RiskLevel, actor.UserID); err != nil {
			return PublishedVersion{}, err
		}
		for _, n := range def.Nodes {
			cfg, _ := json.Marshal(n.Config)
			if len(cfg) == 0 || string(cfg) == "null" {
				cfg = []byte(`{}`)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO workflow_nodes(workflow_id,version,node_id,node_type,name,config,requires_confirmation) VALUES($1,$2,$3,$4,$5,$6,$7)`, rec.Draft.ResourceID, domainVersion, n.ID, n.Type, n.Name, cfg, n.RequiresConfirmation); err != nil {
				return PublishedVersion{}, err
			}
		}
		for _, e := range def.Edges {
			if _, err = tx.Exec(ctx, `INSERT INTO workflow_edges(workflow_id,version,from_node,to_node,condition) VALUES($1,$2,$3,$4,$5)`, rec.Draft.ResourceID, domainVersion, e.From, e.To, e.Condition); err != nil {
				return PublishedVersion{}, err
			}
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO published_resource_pointers(enterprise_id,resource_type,resource_id,change_id,change_version,resource_version,audit_ref_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(enterprise_id,resource_type,resource_id) DO UPDATE SET change_id=EXCLUDED.change_id,change_version=EXCLUDED.change_version,resource_version=EXCLUDED.resource_version,audit_ref_id=EXCLUDED.audit_ref_id,updated_at=now()`, ent, rec.Draft.ResourceType, rec.Draft.ResourceID, rec.Draft.ChangeID, changeVersion, domainVersion, auditRef); err != nil {
		return PublishedVersion{}, err
	}
	stateTag, stateErr := tx.Exec(ctx, `UPDATE change_drafts SET state='published',updated_at=now() WHERE enterprise_id=$1 AND id=$2 AND revision=$3 AND state='approved'`, ent, rec.Draft.ChangeID, rec.Draft.Revision)
	if stateErr != nil {
		return PublishedVersion{}, stateErr
	}
	if stateTag.RowsAffected() != 1 {
		return PublishedVersion{}, ErrConflict
	}
	result := PublishedVersion{ChangeID: rec.Draft.ChangeID, ResourceID: rec.Draft.ResourceID, Version: domainVersion, AuditRefID: auditRef}
	raw, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `UPDATE publish_operations SET status='succeeded',result=$3,finished_at=now() WHERE enterprise_id=$1 AND idempotency_key=$2 AND status='pending'`, ent, idem, raw); err != nil {
		return PublishedVersion{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PublishedVersion{}, err
	}
	return result, nil
}
