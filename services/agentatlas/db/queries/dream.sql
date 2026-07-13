-- name: CreateDreamPolicy :one
INSERT INTO dream_policies (id, enterprise_id, org_scope, status, draft)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ReserveDreamPolicyOperation :one
INSERT INTO dream_policy_operations(enterprise_id,operation_key,operation_kind,policy_id,actor_user_id,request_hash,facts_nonce)
VALUES(sqlc.arg(enterprise_id),sqlc.arg(operation_key),sqlc.arg(operation_kind),sqlc.arg(policy_id),sqlc.arg(actor_user_id),sqlc.arg(request_hash),sqlc.arg(facts_nonce))
ON CONFLICT (enterprise_id,operation_key) DO UPDATE SET operation_key=EXCLUDED.operation_key
WHERE dream_policy_operations.operation_kind=EXCLUDED.operation_kind
  AND dream_policy_operations.policy_id=EXCLUDED.policy_id
  AND dream_policy_operations.actor_user_id=EXCLUDED.actor_user_id
  AND dream_policy_operations.request_hash=EXCLUDED.request_hash
RETURNING *;

-- name: GetDreamPolicyOperation :one
SELECT * FROM dream_policy_operations WHERE enterprise_id=sqlc.arg(enterprise_id) AND operation_key=sqlc.arg(operation_key);

-- name: RecordDreamPolicyOperationAudit :one
UPDATE dream_policy_operations SET audit_ref_id=sqlc.arg(audit_ref_id),updated_at=now()
WHERE enterprise_id=sqlc.arg(enterprise_id) AND operation_key=sqlc.arg(operation_key)
  AND status='pending' AND (audit_ref_id IS NULL OR audit_ref_id=sqlc.arg(audit_ref_id))
RETURNING *;

-- name: CompleteDreamPolicyOperation :one
UPDATE dream_policy_operations SET status='completed',result=sqlc.arg(result),updated_at=now()
WHERE enterprise_id=sqlc.arg(enterprise_id) AND operation_key=sqlc.arg(operation_key)
  AND ((status='pending' AND audit_ref_id IS NOT NULL) OR (status='completed' AND result=sqlc.arg(result)))
RETURNING *;

-- name: GetDreamPolicyTransitionAuditByOperation :one
SELECT * FROM dream_policy_transition_audits
WHERE enterprise_id=sqlc.arg(enterprise_id) AND operation_key=sqlc.arg(operation_key);

-- name: CreateDreamPolicyLifecycle :one
WITH created AS (
  INSERT INTO dream_policies (id, enterprise_id, org_scope, status, draft, requester_user_id, permission_mode, audit_ref_id)
  SELECT sqlc.arg(id),sqlc.arg(enterprise_id),sqlc.arg(org_scope),'draft',sqlc.arg(draft),
         sqlc.arg(requester_user_id),sqlc.arg(permission_mode),op.audit_ref_id
  FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='create' AND op.policy_id=sqlc.arg(id) AND op.actor_user_id=sqlc.arg(requester_user_id)
    AND op.status='pending' AND op.audit_ref_id IS NOT NULL
  RETURNING *
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,0,'create',sqlc.arg(operation_key),audit_ref_id,requester_user_id FROM created
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',result=dream_policy_lifecycle_result(created,0),updated_at=now()
  FROM created JOIN audit ON audit.policy_id=created.id
  WHERE op.enterprise_id=created.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT created.* FROM created JOIN audit ON audit.policy_id=created.id JOIN receipt ON true;

-- name: GetDreamPolicy :one
SELECT * FROM dream_policies WHERE id = $1;

-- name: GetEnterpriseDreamPolicy :one
SELECT * FROM dream_policies WHERE enterprise_id = sqlc.arg(enterprise_id) AND id = sqlc.arg(id);

-- name: AdoptDreamPolicySuggestion :one
WITH op AS (
  SELECT operation.* FROM dream_policy_operations operation
  WHERE operation.enterprise_id=sqlc.arg(enterprise_id) AND operation.operation_key=sqlc.arg(operation_key)
    AND operation.operation_kind='adopt' AND operation.policy_id=sqlc.arg(source_policy_id)
    AND operation.actor_user_id=sqlc.arg(adopter_user_id) AND operation.status='pending'
    AND operation.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), source AS (
  SELECT policy.* FROM dream_policies policy, op
  WHERE policy.enterprise_id=sqlc.arg(enterprise_id) AND policy.id=sqlc.arg(source_policy_id)
    AND policy.permission_mode='suggestion_only' AND policy.status='draft'
    AND policy.revision=sqlc.arg(source_revision)
  FOR UPDATE
), created AS (
  INSERT INTO dream_policies(id,enterprise_id,org_scope,status,draft,requester_user_id,permission_mode,audit_ref_id)
  SELECT sqlc.arg(target_policy_id),source.enterprise_id,source.org_scope,'draft',source.draft,sqlc.arg(adopter_user_id),'direct_edit',sqlc.arg(audit_ref_id)
  FROM source
  RETURNING *
), lineage AS (
  INSERT INTO dream_policy_adoptions(enterprise_id,source_policy_id,source_requester_user_id,source_revision,target_policy_id,adopter_user_id,audit_ref_id,operation_key)
  SELECT source.enterprise_id,source.id,source.requester_user_id,source.revision,created.id,sqlc.arg(adopter_user_id),sqlc.arg(audit_ref_id),sqlc.arg(operation_key)
  FROM source JOIN created ON true
  RETURNING target_policy_id
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT created.enterprise_id,created.id,0,'adopt',sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(adopter_user_id)
  FROM created JOIN lineage ON lineage.target_policy_id=created.id
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations operation SET status='completed',result=dream_policy_lifecycle_result(created,0),updated_at=now()
  FROM created JOIN audit ON audit.policy_id=created.id
  WHERE operation.enterprise_id=created.enterprise_id AND operation.operation_key=sqlc.arg(operation_key)
  RETURNING operation.operation_key
)
SELECT created.* FROM created JOIN audit ON audit.policy_id=created.id JOIN receipt ON true;

-- name: GetDreamPolicyAdoptionBySource :one
SELECT * FROM dream_policy_adoptions
WHERE enterprise_id=sqlc.arg(enterprise_id) AND source_policy_id=sqlc.arg(source_policy_id)
  AND source_revision=sqlc.arg(source_revision);

-- name: UpdateDreamPolicyDraftIfRevision :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='update' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET org_scope=sqlc.arg(org_scope), draft=sqlc.arg(draft), revision=revision+1,
      status='draft', requester_user_id=CASE WHEN requester_user_id='' THEN sqlc.arg(actor_user_id) ELSE requester_user_id END,
      pending_action='',review_state='',risk_level='', risk_reasons='[]', review_mode='', reviewer_user_id=NULL,
      review_org_path='[]', review_queue=NULL, decision='', audit_ref_id=sqlc.arg(audit_ref_id), updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.status IN ('draft','rejected','published','disabled')
    AND dream_policies.permission_mode='direct_edit'
  RETURNING dream_policies.*
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'update',sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',
    result=dream_policy_lifecycle_result(changed,COALESCE((SELECT max(version) FROM dream_policy_versions WHERE policy_id=changed.id),0)::integer),updated_at=now()
  FROM changed JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT changed.* FROM changed JOIN audit ON audit.policy_id=changed.id JOIN receipt ON true;

-- name: SubmitDreamPolicyReviewIfRevision :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='review' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET pending_action=sqlc.arg(pending_action),review_state='pending', risk_level=sqlc.arg(risk_level),
      risk_reasons=sqlc.arg(risk_reasons), review_mode=sqlc.arg(review_mode),
      reviewer_user_id=sqlc.narg(reviewer_user_id), review_org_path=sqlc.arg(review_org_path),
      review_queue=sqlc.narg(review_queue), decision='', audit_ref_id=sqlc.arg(audit_ref_id), updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.permission_mode='direct_edit'
    AND ((sqlc.arg(pending_action)::text='publish' AND dream_policies.status='draft') OR (sqlc.arg(pending_action)::text='disable' AND dream_policies.status='published'))
    AND dream_policies.review_state IN ('','rejected')
  RETURNING dream_policies.*
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'review:'||pending_action,sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',
    result=dream_policy_lifecycle_result(changed,COALESCE((SELECT max(version) FROM dream_policy_versions WHERE policy_id=changed.id),0)::integer),updated_at=now()
  FROM changed JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT changed.* FROM changed JOIN audit ON audit.policy_id=changed.id JOIN receipt ON true;

-- name: RefreshDreamPolicyReviewRoute :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='review' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET risk_level=sqlc.arg(risk_level),risk_reasons=sqlc.arg(risk_reasons),review_mode=sqlc.arg(review_mode),
    reviewer_user_id=sqlc.narg(reviewer_user_id),review_org_path=sqlc.arg(review_org_path),review_queue=sqlc.narg(review_queue),
    audit_ref_id=sqlc.arg(audit_ref_id),updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.review_state='pending' AND dream_policies.review_mode='enterprise_knowledge_admin_queue'
  RETURNING dream_policies.*
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'review-refresh:'||pending_action,sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',
    result=dream_policy_lifecycle_result(changed,COALESCE((SELECT max(version) FROM dream_policy_versions WHERE policy_id=changed.id),0)::integer),updated_at=now()
  FROM changed JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT changed.* FROM changed JOIN audit ON audit.policy_id=changed.id JOIN receipt ON true;

-- name: DecideDreamPolicyIfRevision :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='decision' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)::text
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET review_state=CASE WHEN sqlc.arg(decision)::text='approve' THEN 'approved' ELSE 'rejected' END,
      decision=sqlc.arg(decision), audit_ref_id=sqlc.arg(audit_ref_id), updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.review_state='pending'
    AND ((dream_policies.review_mode='upward_review' AND dream_policies.reviewer_user_id=sqlc.arg(actor_user_id) AND dream_policies.requester_user_id<>sqlc.arg(actor_user_id))
      OR (dream_policies.risk_level='low' AND dream_policies.requester_user_id=sqlc.arg(actor_user_id) AND dream_policies.review_mode='single_confirmation'))
  RETURNING dream_policies.*
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'decision:'||pending_action||':'||sqlc.arg(decision),sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',
    result=dream_policy_lifecycle_result(changed,COALESCE((SELECT max(version) FROM dream_policy_versions WHERE policy_id=changed.id),0)::integer),updated_at=now()
  FROM changed JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT changed.* FROM changed JOIN audit ON audit.policy_id=changed.id JOIN receipt ON true;

-- name: PublishDreamPolicyGoverned :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='publish' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET status='published', revision=revision+1, pending_action='',review_state='',audit_ref_id=sqlc.arg(audit_ref_id), updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.status='draft' AND dream_policies.review_state='approved' AND dream_policies.pending_action='publish' AND dream_policies.permission_mode='direct_edit'
  RETURNING dream_policies.*
), inserted AS (
  INSERT INTO dream_policy_versions(policy_id,version,definition)
  SELECT id, COALESCE((SELECT max(version)+1 FROM dream_policy_versions WHERE policy_id=id),1), draft FROM changed
  RETURNING *
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'publish',sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',result=dream_policy_lifecycle_result(changed,inserted.version),updated_at=now()
  FROM changed JOIN inserted ON inserted.policy_id=changed.id JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT inserted.* FROM inserted JOIN audit ON audit.policy_id=inserted.policy_id JOIN receipt ON true;

-- name: DisableDreamPolicyIfRevision :one
WITH op AS (
  SELECT * FROM dream_policy_operations op
  WHERE op.enterprise_id=sqlc.arg(target_enterprise_id) AND op.operation_key=sqlc.arg(operation_key)
    AND op.operation_kind='disable' AND op.policy_id=sqlc.arg(target_id) AND op.actor_user_id=sqlc.arg(actor_user_id)
    AND op.status='pending' AND op.audit_ref_id=sqlc.arg(audit_ref_id)
  FOR UPDATE
), changed AS (
  UPDATE dream_policies SET status='disabled', revision=revision+1,pending_action='',review_state='',audit_ref_id=sqlc.arg(audit_ref_id), updated_at=now()
  FROM op WHERE dream_policies.enterprise_id=sqlc.arg(target_enterprise_id) AND dream_policies.id=sqlc.arg(target_id)
    AND dream_policies.revision=sqlc.arg(expected_revision) AND dream_policies.status='published' AND dream_policies.review_state='approved' AND dream_policies.pending_action='disable' AND dream_policies.permission_mode='direct_edit'
  RETURNING dream_policies.*
), audit AS (
  INSERT INTO dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id)
  SELECT enterprise_id,id,revision,'disable',sqlc.arg(operation_key),sqlc.arg(audit_ref_id),sqlc.arg(actor_user_id) FROM changed
  RETURNING policy_id
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',
    result=dream_policy_lifecycle_result(changed,COALESCE((SELECT max(version) FROM dream_policy_versions WHERE policy_id=changed.id),0)::integer),updated_at=now()
  FROM changed JOIN audit ON audit.policy_id=changed.id
  WHERE op.enterprise_id=changed.enterprise_id AND op.operation_key=sqlc.arg(operation_key) AND op.status='pending'
  RETURNING op.operation_key
)
SELECT changed.* FROM changed JOIN audit ON audit.policy_id=changed.id JOIN receipt ON true;

-- name: UpdateDreamPolicyStatus :execrows
UPDATE dream_policies SET status = $2, updated_at = now() WHERE id = $1;

-- name: PublishDreamPolicyVersion :one
INSERT INTO dream_policy_versions (policy_id, version, definition)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetLatestDreamPolicyVersion :one
SELECT * FROM dream_policy_versions WHERE policy_id = $1 ORDER BY version DESC LIMIT 1;

-- name: GetDreamPolicyVersion :one
SELECT * FROM dream_policy_versions WHERE policy_id = sqlc.arg(policy_id) AND version = sqlc.arg(version);

-- name: ListPublishedDreamPolicies :many
SELECT * FROM dream_policies WHERE enterprise_id = $1 AND status = 'published' ORDER BY id;

-- name: ListPublishedDreamPoliciesBounded :many
SELECT * FROM dream_policies WHERE enterprise_id=sqlc.arg(enterprise_id) AND status='published' ORDER BY id LIMIT sqlc.arg(result_limit);

-- name: ListDreamPolicyLifecyclesByOrgBounded :many
SELECT * FROM dream_policies
WHERE enterprise_id=sqlc.arg(enterprise_id) AND org_scope=sqlc.arg(org_scope)
ORDER BY updated_at DESC, id
LIMIT sqlc.arg(result_limit);

-- name: CreateDreamRun :one
WITH eligible AS (
  SELECT true AS allowed
  WHERE COALESCE(NULLIF(sqlc.arg(operation_kind)::text, ''), 'scheduled') NOT IN ('manual_rerun','backfill')
     OR EXISTS (
       SELECT 1 FROM dream_policy_operations op
       WHERE op.enterprise_id=sqlc.arg(enterprise_id) AND op.operation_key=sqlc.arg(idempotency_key)
         AND op.status='pending' AND op.audit_ref_id=sqlc.narg(audit_ref_id)
         AND ((sqlc.arg(operation_kind)::text='manual_rerun' AND op.operation_kind='rerun' AND op.policy_id=sqlc.narg(rerun_of_run_id))
           OR (sqlc.arg(operation_kind)::text='backfill' AND op.operation_kind='backfill' AND op.policy_id=sqlc.arg(policy_id)))
     )
), inserted AS (
INSERT INTO dream_runs (
    id, policy_id, version, enterprise_id, status, window_start, window_end,
    org_unit_id, policy_version, workflow_id, workflow_version, timezone,
    input_snapshot, visibility_snapshot, model_route, model_version, attempt,
    rerun_of_run_id, coverage, missing_inputs, idempotency_key, org_version,
    operation_kind, audit_ref_id
)
SELECT
    sqlc.arg(id), sqlc.arg(policy_id), sqlc.arg(version), sqlc.arg(enterprise_id),
    sqlc.arg(status), sqlc.arg(window_start), sqlc.arg(window_end),
    sqlc.arg(org_unit_id), sqlc.arg(policy_version), sqlc.arg(workflow_id)::text,
    sqlc.arg(workflow_version)::integer, sqlc.arg(timezone),
    sqlc.arg(input_snapshot), sqlc.arg(visibility_snapshot), sqlc.arg(model_route),
    sqlc.arg(model_version), sqlc.arg(attempt), sqlc.narg(rerun_of_run_id),
    sqlc.arg(coverage), sqlc.arg(missing_inputs), sqlc.arg(idempotency_key),
    COALESCE(NULLIF(sqlc.arg(org_version)::bigint, 0), 1),
    COALESCE(NULLIF(sqlc.arg(operation_kind)::text, ''), 'scheduled'), sqlc.narg(audit_ref_id)
FROM eligible
ON CONFLICT DO NOTHING
RETURNING *
), receipt AS (
  UPDATE dream_policy_operations op SET status='completed',result=jsonb_build_object('run_id',inserted.id),updated_at=now()
  FROM inserted
  WHERE inserted.operation_kind IN ('manual_rerun','backfill')
    AND op.enterprise_id=inserted.enterprise_id AND op.operation_key=inserted.idempotency_key AND op.status='pending'
  RETURNING op.operation_key
)
SELECT inserted.* FROM inserted
WHERE inserted.operation_kind NOT IN ('manual_rerun','backfill') OR EXISTS (SELECT 1 FROM receipt);

-- name: GetDreamRun :one
SELECT * FROM dream_runs WHERE id = $1;

-- name: BindDreamWorkflowRun :one
UPDATE dream_runs SET workflow_run_id = sqlc.arg(workflow_run_id)
WHERE enterprise_id = sqlc.arg(enterprise_id) AND id = sqlc.arg(run_id)
  AND (workflow_run_id IS NULL OR workflow_run_id = sqlc.arg(workflow_run_id))
RETURNING *;

-- name: PublishDreamWorkflowWait :one
UPDATE dream_runs SET workflow_run_id=sqlc.arg(workflow_run_id), status='waiting_confirmation', error='',
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND status IN ('running','waiting_confirmation') AND (workflow_run_id IS NULL OR workflow_run_id=sqlc.arg(workflow_run_id))
RETURNING *;

-- name: GetDreamRunByWorkflowRun :one
SELECT * FROM dream_runs WHERE enterprise_id = sqlc.arg(enterprise_id) AND workflow_run_id = sqlc.arg(workflow_run_id);

-- name: RequeueDreamRunAfterWorkflow :one
UPDATE dream_runs SET status = 'pending', error = '', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id = sqlc.arg(enterprise_id) AND workflow_run_id = sqlc.arg(workflow_run_id)
  AND status IN ('waiting_confirmation','pending')
RETURNING *;

-- name: FailDreamRunAfterWorkflow :one
UPDATE dream_runs SET status='failed', error=sqlc.arg(error), finished_at=COALESCE(finished_at, now()),
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND workflow_run_id=sqlc.arg(workflow_run_id)
  AND status IN ('running','waiting_confirmation','pending','failed')
RETURNING *;

-- name: ListPendingDreamWorkflowLifecycle :many
SELECT * FROM dream_workflow_lifecycle_outbox
WHERE processed_at IS NULL
ORDER BY id
LIMIT sqlc.arg(result_limit);

-- name: RecordDreamWorkflowLifecycleFailure :exec
UPDATE dream_workflow_lifecycle_outbox
SET attempts = attempts + 1, last_error = sqlc.arg(last_error), updated_at = now()
WHERE id = sqlc.arg(id) AND processed_at IS NULL;

-- name: CompleteDreamWorkflowLifecycle :exec
UPDATE dream_workflow_lifecycle_outbox
SET processed_at = now(), last_error = '', updated_at = now()
WHERE id = sqlc.arg(id) AND processed_at IS NULL;

-- name: ReserveDreamOutputHash :one
UPDATE dream_runs SET output_hash=sqlc.arg(output_hash)
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND (output_hash IS NULL OR output_hash=sqlc.arg(output_hash))
RETURNING *;

-- name: ReserveDreamOutputHashOwned :one
UPDATE dream_runs SET output_hash=sqlc.arg(output_hash)
WHERE enterprise_id=sqlc.arg(enterprise_id) AND id=sqlc.arg(run_id)
  AND execution_owner=sqlc.arg(execution_owner) AND execution_lease_expires_at > now()
  AND status='running' AND (output_hash IS NULL OR output_hash=sqlc.arg(output_hash))
RETURNING *;

-- name: FenceDreamExecutionOwner :one
SELECT id FROM dream_runs
WHERE id=sqlc.arg(id) AND execution_owner=sqlc.arg(execution_owner)
  AND execution_lease_expires_at > now() AND status='running'
FOR UPDATE;

-- name: ClaimDreamRun :execrows
UPDATE dream_runs SET status = 'running' WHERE id = $1 AND status = 'pending';

-- name: ClaimDreamRunLease :execrows
UPDATE dream_runs
SET status='running', execution_owner=sqlc.arg(execution_owner),
    execution_lease_expires_at=now()+interval '2 minutes'
WHERE id=sqlc.arg(id) AND status='pending';

-- name: RenewDreamRunLease :execrows
UPDATE dream_runs
SET execution_lease_expires_at=now()+interval '2 minutes'
WHERE id=sqlc.arg(id) AND status='running' AND execution_owner=sqlc.arg(execution_owner);

-- name: RecoverExpiredDreamRunAfterWorkflow :one
UPDATE dream_runs
SET status='pending', error='', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE enterprise_id=sqlc.arg(enterprise_id) AND workflow_run_id=sqlc.arg(workflow_run_id)
  AND status='running' AND (execution_lease_expires_at IS NULL OR execution_lease_expires_at <= now())
RETURNING *;

-- name: RecoverExpiredUnboundDreamRuns :many
UPDATE dream_runs
SET status='pending', error='', execution_owner=NULL, execution_lease_expires_at=NULL
WHERE id IN (
    SELECT id FROM dream_runs
    WHERE status='running' AND workflow_run_id IS NULL
      AND (execution_lease_expires_at IS NULL OR execution_lease_expires_at <= now())
    ORDER BY created_at
    LIMIT sqlc.arg(result_limit)
    FOR UPDATE SKIP LOCKED
)
RETURNING id;

-- name: ListPendingDreamRuns :many
SELECT id FROM dream_runs WHERE status='pending' ORDER BY created_at LIMIT sqlc.arg(result_limit);

-- name: GetLatestDreamRunForPolicy :one
SELECT * FROM dream_runs
WHERE policy_id = $1 AND rerun_of_run_id IS NULL
  AND operation_kind IN ('scheduled','automatic_retry')
ORDER BY window_end DESC, attempt DESC, created_at DESC, id DESC
LIMIT 1;

-- name: GetLatestDreamRunForPolicyVersion :one
SELECT * FROM dream_runs
WHERE policy_id = sqlc.arg(policy_id)
  AND policy_version = sqlc.arg(policy_version)
  AND rerun_of_run_id IS NULL
  AND operation_kind IN ('scheduled','automatic_retry')
ORDER BY window_end DESC, attempt DESC, created_at DESC, id DESC
LIMIT 1;

-- name: GetDreamRunByIdempotencyKey :one
SELECT * FROM dream_runs
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: GetDreamOrgTreeVersion :one
SELECT COALESCE(max(org_version), 0)::bigint
FROM knowledge_spaces
WHERE enterprise_id = sqlc.arg(enterprise_id);

-- name: UpdateDreamRunStatus :execrows
UPDATE dream_runs
SET status = $2, error = $3,
    finished_at = CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END,
    execution_owner = CASE WHEN $2 IN ('succeeded','failed') THEN NULL ELSE execution_owner END,
    execution_lease_expires_at = CASE WHEN $2 IN ('succeeded','failed') THEN NULL ELSE execution_lease_expires_at END
WHERE id = $1;

-- name: CompleteDreamRunOwned :execrows
UPDATE dream_runs
SET status=sqlc.arg(status), error=sqlc.arg(error), finished_at=now(),
    execution_owner=NULL, execution_lease_expires_at=NULL
WHERE id=sqlc.arg(id) AND status='running'
  AND execution_owner=sqlc.arg(execution_owner) AND execution_lease_expires_at > now()
  AND sqlc.arg(status) IN ('succeeded','failed');

-- name: InsertDreamInput :exec
INSERT INTO dream_inputs (run_id, source_type, source_id)
VALUES ($1, $2, $3)
ON CONFLICT (run_id, source_type, source_id) DO NOTHING;

-- name: ListDreamInputsForRun :many
SELECT * FROM dream_inputs WHERE run_id = sqlc.arg(run_id) ORDER BY source_type, source_id;

-- name: CreateDreamSummary :one
INSERT INTO dream_summaries (id, run_id, enterprise_id, space_id, layer, summary_text, sealed_object_key, evidence_pointer_id, risk_signals, facts, themes, trends, todos)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
        COALESCE(sqlc.narg(risk_signals)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(facts)::jsonb, '[]'::jsonb),
        COALESCE(sqlc.narg(themes)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(trends)::jsonb, '[]'::jsonb), COALESCE(sqlc.narg(todos)::jsonb, '[]'::jsonb))
RETURNING *;

-- name: GetDreamSummary :one
SELECT * FROM dream_summaries WHERE id = $1;

-- name: ListDreamSummariesBySpace :many
SELECT * FROM dream_summaries
WHERE space_id = $1 AND layer = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: GetDreamSummaryForRunLayer :one
SELECT * FROM dream_summaries
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND run_id = sqlc.arg(run_id)
  AND space_id = sqlc.arg(space_id)
  AND layer = sqlc.arg(layer)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: InsertDreamEvidencePointer :exec
INSERT INTO dream_evidence_pointers (dream_summary_id, evidence_pointer_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListChildSpaces :many
SELECT DISTINCT spaces.*
FROM knowledge_spaces AS spaces
JOIN org_scope_bindings AS bindings ON bindings.space_id = spaces.id
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE spaces.enterprise_id = sqlc.arg(enterprise_id)
  AND bindings.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
ORDER BY spaces.kind, spaces.name, spaces.id;

-- name: ListDreamImmediateChildren :many
SELECT DISTINCT spaces.*,
       parent_space.id::text AS parent_space_id,
       parent_binding.scope_kind::text AS parent_scope_kind,
       parent_binding.scope_id::text AS parent_scope_id,
       parent_space.org_scope::text AS parent_org_scope
FROM knowledge_spaces AS spaces
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = spaces.enterprise_id
 AND bindings.space_id = spaces.id
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.enterprise_id = parent_binding.enterprise_id
 AND parent_space.id = parent_binding.space_id
WHERE spaces.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
ORDER BY spaces.kind, spaces.name, spaces.id, parent_space.id,
         parent_binding.scope_kind, parent_binding.scope_id, parent_space.org_scope
LIMIT sqlc.arg(result_limit);

-- name: ListCompletedChildDreamRuns :many
SELECT runs.*
FROM dream_runs AS runs
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = runs.enterprise_id
JOIN knowledge_spaces AS child_space
  ON child_space.id = bindings.space_id
 AND child_space.enterprise_id = runs.enterprise_id
 AND (bindings.scope_id = runs.org_unit_id OR child_space.org_scope = runs.org_unit_id)
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
  AND runs.status = 'succeeded'
  AND runs.window_start = sqlc.arg(window_start)
  AND runs.window_end = sqlc.arg(window_end)
ORDER BY runs.org_unit_id, runs.id;

-- name: ListDreamCompletedChildRuns :many
SELECT DISTINCT runs.*,
       child_space.id::text AS child_space_id,
       child_space.org_scope::text AS child_org_scope,
       parent_space.id::text AS parent_space_id,
       parent_binding.scope_kind::text AS parent_scope_kind,
       parent_binding.scope_id::text AS parent_scope_id,
       parent_space.org_scope::text AS parent_org_scope
FROM dream_runs AS runs
JOIN org_scope_bindings AS bindings
  ON bindings.enterprise_id = runs.enterprise_id
JOIN knowledge_spaces AS child_space
  ON child_space.id = bindings.space_id
 AND child_space.enterprise_id = runs.enterprise_id
 AND (bindings.scope_id = runs.org_unit_id OR child_space.org_scope = runs.org_unit_id)
JOIN org_scope_bindings AS parent_binding
  ON parent_binding.enterprise_id = bindings.enterprise_id
 AND parent_binding.scope_kind = bindings.parent_scope_kind
 AND parent_binding.scope_id = bindings.parent_scope_id
JOIN knowledge_spaces AS parent_space
  ON parent_space.id = parent_binding.space_id
 AND parent_space.enterprise_id = bindings.enterprise_id
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND parent_binding.scope_kind = sqlc.arg(parent_scope_kind)::text
  AND parent_binding.scope_id = sqlc.arg(parent_scope_id)::text
  AND runs.status = 'succeeded'
  AND runs.window_start = sqlc.arg(window_start)
  AND runs.window_end = sqlc.arg(window_end)
ORDER BY runs.org_unit_id, runs.id, child_space.id, child_space.org_scope,
         parent_space.id, parent_binding.scope_kind, parent_binding.scope_id, parent_space.org_scope
LIMIT sqlc.arg(result_limit);

-- name: ListDreamRunsByOrg :many
SELECT *
FROM dream_runs
WHERE enterprise_id = sqlc.arg(enterprise_id)
  AND org_unit_id = sqlc.arg(org_unit_id)
ORDER BY window_end DESC, id DESC
LIMIT sqlc.arg(result_limit);

-- name: GetDreamRunView :one
SELECT runs.*,
       ARRAY(
           SELECT lineage.parent_run_id
           FROM dream_run_lineage AS lineage
           WHERE lineage.run_id = runs.id AND lineage.relation = 'child_summary'
           ORDER BY lineage.parent_run_id
       )::text[] AS parent_run_ids,
       (SELECT count(*) FROM dream_inputs AS inputs WHERE inputs.run_id = runs.id) AS input_count,
       COALESCE(summary.summary_text, '') AS display_summary,
       COALESCE(summary.facts, '[]'::jsonb) AS facts,
       COALESCE(summary.themes, '[]'::jsonb) AS themes,
       COALESCE(summary.trends, '[]'::jsonb) AS trends,
       COALESCE(summary.risk_signals, '[]'::jsonb) AS risks,
       COALESCE(summary.todos, '[]'::jsonb) AS todos,
       sealed.evidence_pointer_id
FROM dream_runs AS runs
LEFT JOIN LATERAL (
    SELECT dream_summaries.*
    FROM dream_summaries
    WHERE dream_summaries.run_id = runs.id
      AND dream_summaries.enterprise_id = runs.enterprise_id
      AND dream_summaries.layer = 'display'
      AND runs.status = 'succeeded'
    ORDER BY dream_summaries.created_at DESC, dream_summaries.id DESC
    LIMIT 1
) AS summary ON true
LEFT JOIN LATERAL (
    SELECT dream_summaries.evidence_pointer_id
    FROM dream_summaries
    WHERE dream_summaries.run_id = runs.id
      AND dream_summaries.enterprise_id = runs.enterprise_id
      AND dream_summaries.layer = 'sealed_pointer'
      AND runs.status = 'succeeded'
    ORDER BY dream_summaries.created_at DESC, dream_summaries.id DESC
    LIMIT 1
) AS sealed ON true
WHERE runs.enterprise_id = sqlc.arg(enterprise_id)
  AND runs.id = sqlc.arg(run_id);

-- name: CreateDreamRunLineage :one
INSERT INTO dream_run_lineage (run_id, parent_run_id, relation)
VALUES (sqlc.arg(run_id), sqlc.arg(parent_run_id), sqlc.arg(relation))
RETURNING *;

-- name: CreateDreamAnnotation :one
INSERT INTO dream_run_annotations (
    id, enterprise_id, run_id, annotation_type, body, created_by
)
VALUES (
    sqlc.arg(id), sqlc.arg(enterprise_id), sqlc.arg(run_id),
    sqlc.arg(annotation_type), sqlc.arg(body), sqlc.arg(created_by)
)
RETURNING *;

-- name: ListDreamRunAnnotationsByRunBounded :many
SELECT * FROM dream_run_annotations
WHERE enterprise_id = sqlc.arg(enterprise_id) AND run_id = sqlc.arg(run_id)
ORDER BY created_at, id
LIMIT sqlc.arg(result_limit);

-- name: ListDreamRunChildrenByParentBounded :many
SELECT child.* FROM dream_run_lineage lineage
JOIN dream_runs child ON child.id = lineage.run_id
WHERE child.enterprise_id = sqlc.arg(enterprise_id)
  AND lineage.parent_run_id = sqlc.arg(parent_run_id)
ORDER BY child.created_at, child.id
LIMIT sqlc.arg(result_limit);
