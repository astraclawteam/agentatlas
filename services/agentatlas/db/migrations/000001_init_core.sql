-- +goose Up

-- Data boundary invariant: no table stores raw originals, full OCR, full
-- transcripts, or long unmasked chunks. Text columns carry length checks;
-- full parse intermediates live in object storage referenced by object_key.

CREATE TABLE enterprises (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ── knowledge spaces ────────────────────────────────────────────────────────

CREATE TABLE knowledge_spaces (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    kind          text NOT NULL CHECK (kind IN ('employee','project_group','department','business_unit','company')),
    name          text NOT NULL,
    org_scope     text NOT NULL,
    org_version   bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, org_scope)
);

CREATE TABLE knowledge_space_versions (
    id          bigserial PRIMARY KEY,
    space_id    text NOT NULL REFERENCES knowledge_spaces(id),
    org_version bigint NOT NULL,
    snapshot    jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_scope_bindings (
    id              bigserial PRIMARY KEY,
    enterprise_id   text NOT NULL REFERENCES enterprises(id),
    space_id        text NOT NULL REFERENCES knowledge_spaces(id),
    scope_kind      text NOT NULL CHECK (scope_kind IN ('employee','project_group','department','business_unit','company')),
    scope_id        text NOT NULL,
    parent_scope_id text,
    UNIQUE (enterprise_id, scope_kind, scope_id)
);

CREATE TABLE org_snapshots (
    id            bigserial PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    org_version   bigint NOT NULL,
    snapshot      jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, org_version)
);

CREATE TABLE space_membership_cache (
    space_id     text NOT NULL REFERENCES knowledge_spaces(id),
    user_id      text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    org_version  bigint NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, user_id)
);

-- ── workflows ───────────────────────────────────────────────────────────────

CREATE TABLE workflows (
    id               text PRIMARY KEY,
    enterprise_id    text NOT NULL REFERENCES enterprises(id),
    name             text NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('sop','dream','ingestion','answer')),
    created_by       text NOT NULL DEFAULT '',
    draft            jsonb NOT NULL,
    draft_updated_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workflow_versions (
    workflow_id  text NOT NULL REFERENCES workflows(id),
    version      integer NOT NULL CHECK (version > 0),
    definition   jsonb NOT NULL,
    risk_level   text NOT NULL CHECK (risk_level IN ('low','medium','high')),
    published_by text NOT NULL DEFAULT '',
    published_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, version)
);

CREATE TABLE workflow_nodes (
    workflow_id           text NOT NULL,
    version               integer NOT NULL,
    node_id               text NOT NULL,
    node_type             text NOT NULL,
    name                  text NOT NULL DEFAULT '',
    config                jsonb NOT NULL DEFAULT '{}',
    requires_confirmation boolean NOT NULL DEFAULT false,
    PRIMARY KEY (workflow_id, version, node_id),
    FOREIGN KEY (workflow_id, version) REFERENCES workflow_versions(workflow_id, version)
);

CREATE TABLE workflow_edges (
    workflow_id text NOT NULL,
    version     integer NOT NULL,
    from_node   text NOT NULL,
    to_node     text NOT NULL,
    condition   text NOT NULL DEFAULT '',
    PRIMARY KEY (workflow_id, version, from_node, to_node),
    FOREIGN KEY (workflow_id, version) REFERENCES workflow_versions(workflow_id, version)
);

CREATE TABLE workflow_runs (
    id            text PRIMARY KEY,
    workflow_id   text NOT NULL REFERENCES workflows(id),
    version       integer NOT NULL,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    status        text NOT NULL CHECK (status IN ('pending','running','waiting_confirmation','succeeded','failed','cancelled')),
    input         jsonb NOT NULL DEFAULT '{}',
    output        jsonb,
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

CREATE TABLE workflow_run_events (
    id          bigserial PRIMARY KEY,
    run_id      text NOT NULL REFERENCES workflow_runs(id),
    node_id     text NOT NULL DEFAULT '',
    status      text NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workflow_artifacts (
    id          bigserial PRIMARY KEY,
    run_id      text NOT NULL REFERENCES workflow_runs(id),
    artifact_id text NOT NULL,
    role        text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- ── SOP and methods ─────────────────────────────────────────────────────────

CREATE TABLE sops (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    title         text NOT NULL,
    org_scope     text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sop_versions (
    sop_id                   text NOT NULL REFERENCES sops(id),
    version                  integer NOT NULL CHECK (version > 0),
    structure                jsonb NOT NULL,
    source_atlas_document_id text,
    published_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sop_id, version)
);

CREATE TABLE method_outlines (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    title         text NOT NULL,
    outline       jsonb NOT NULL,
    org_scope     text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE method_outline_versions (
    outline_id   text NOT NULL REFERENCES method_outlines(id),
    version      integer NOT NULL CHECK (version > 0),
    outline      jsonb NOT NULL,
    published_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (outline_id, version)
);

CREATE TABLE sop_workflow_bindings (
    sop_id      text NOT NULL REFERENCES sops(id),
    workflow_id text NOT NULL REFERENCES workflows(id),
    version     integer NOT NULL,
    PRIMARY KEY (sop_id, workflow_id)
);

CREATE TABLE method_space_bindings (
    outline_id text NOT NULL REFERENCES method_outlines(id),
    space_id   text NOT NULL REFERENCES knowledge_spaces(id),
    PRIMARY KEY (outline_id, space_id)
);

-- ── evidence pointers (referenced widely, created before dependents) ───────

CREATE TABLE evidence_pointers (
    id                      text PRIMARY KEY,
    enterprise_id           text NOT NULL REFERENCES enterprises(id),
    resource_type           text NOT NULL,
    resource_ref            text NOT NULL,
    source_system           text NOT NULL,
    content_hash            text NOT NULL DEFAULT '',
    summary_hash            text NOT NULL DEFAULT '',
    agentnexus_resource_uri text NOT NULL DEFAULT '',
    required_scopes         text[] NOT NULL DEFAULT '{}',
    created_at              timestamptz NOT NULL DEFAULT now()
);

-- ── artifacts and parsing ───────────────────────────────────────────────────

CREATE TABLE artifacts (
    id                  text PRIMARY KEY,
    enterprise_id       text NOT NULL REFERENCES enterprises(id),
    filename            text NOT NULL DEFAULT '',
    content_type        text NOT NULL DEFAULT '',
    object_key          text NOT NULL DEFAULT '',
    source_hash         text NOT NULL DEFAULT '',
    evidence_pointer_id text REFERENCES evidence_pointers(id),
    size_bytes          bigint NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifact_processing_jobs (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    artifact_id   text NOT NULL REFERENCES artifacts(id),
    status        text NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    parser_hint   text NOT NULL DEFAULT 'auto',
    error         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz
);

CREATE TABLE artifact_processing_steps (
    id          bigserial PRIMARY KEY,
    job_id      text NOT NULL REFERENCES artifact_processing_jobs(id),
    step        text NOT NULL,
    status      text NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE atlas_documents (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    artifact_id   text NOT NULL REFERENCES artifacts(id),
    source_hash   text NOT NULL,
    content_type  text NOT NULL DEFAULT '',
    provider      text NOT NULL DEFAULT '',
    -- Full AtlasDocument JSON lives in object storage, never in this table.
    object_key    text NOT NULL,
    confidence    real NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE document_blocks (
    atlas_document_id text NOT NULL REFERENCES atlas_documents(id),
    block_id          text NOT NULL,
    block_type        text NOT NULL,
    page              integer,
    block_order       integer NOT NULL DEFAULT 0,
    text_hash         text NOT NULL DEFAULT '',
    sanitized_excerpt text NOT NULL DEFAULT '' CHECK (char_length(sanitized_excerpt) <= 512),
    PRIMARY KEY (atlas_document_id, block_id)
);

CREATE TABLE document_summaries (
    id                bigserial PRIMARY KEY,
    atlas_document_id text NOT NULL REFERENCES atlas_documents(id),
    level             text NOT NULL CHECK (level IN ('section','document','display','retrieval')),
    ref               text NOT NULL DEFAULT '',
    summary_text      text NOT NULL CHECK (char_length(summary_text) <= 4000),
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE parser_provider_runs (
    id          bigserial PRIMARY KEY,
    job_id      text NOT NULL REFERENCES artifact_processing_jobs(id),
    provider_id text NOT NULL,
    status      text NOT NULL,
    latency_ms  integer NOT NULL DEFAULT 0,
    confidence  real NOT NULL DEFAULT 0,
    error       text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

-- ── retrieval and indexing ──────────────────────────────────────────────────

CREATE TABLE retrieval_plans (
    id              text PRIMARY KEY,
    enterprise_id   text NOT NULL REFERENCES enterprises(id),
    query_hash      text NOT NULL,
    sanitized_query text NOT NULL DEFAULT '' CHECK (char_length(sanitized_query) <= 1000),
    space_ids       text[] NOT NULL DEFAULT '{}',
    org_scopes      text[] NOT NULL DEFAULT '{}',
    filters         jsonb NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE retrieval_plan_steps (
    plan_id text NOT NULL REFERENCES retrieval_plans(id),
    step_no integer NOT NULL,
    kind    text NOT NULL CHECK (kind IN ('keyword','vector','filter','rerank','nexus_locate','nexus_read')),
    params  jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (plan_id, step_no)
);

CREATE TABLE retrieval_results (
    id                  bigserial PRIMARY KEY,
    plan_id             text NOT NULL REFERENCES retrieval_plans(id),
    evidence_pointer_id text REFERENCES evidence_pointers(id),
    doc_ref             text NOT NULL DEFAULT '',
    score               real NOT NULL DEFAULT 0,
    snippet             text NOT NULL DEFAULT '' CHECK (char_length(snippet) <= 1000),
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE index_jobs (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    source_type   text NOT NULL,
    source_id     text NOT NULL,
    status        text NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    error         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

CREATE TABLE index_documents (
    id                  text PRIMARY KEY,
    enterprise_id       text NOT NULL REFERENCES enterprises(id),
    index_name          text NOT NULL,
    source_type         text NOT NULL,
    source_id           text NOT NULL,
    evidence_pointer_id text REFERENCES evidence_pointers(id),
    org_version         bigint NOT NULL DEFAULT 0,
    indexed_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (index_name, source_type, source_id)
);

-- ── dreams ──────────────────────────────────────────────────────────────────

CREATE TABLE dream_policies (
    id            text PRIMARY KEY,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    org_scope     text NOT NULL,
    status        text NOT NULL CHECK (status IN ('draft','published','disabled')),
    draft         jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dream_policy_versions (
    policy_id    text NOT NULL REFERENCES dream_policies(id),
    version      integer NOT NULL CHECK (version > 0),
    definition   jsonb NOT NULL,
    published_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (policy_id, version)
);

CREATE TABLE dream_runs (
    id            text PRIMARY KEY,
    policy_id     text NOT NULL REFERENCES dream_policies(id),
    version       integer NOT NULL,
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    status        text NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    window_start  timestamptz NOT NULL,
    window_end    timestamptz NOT NULL,
    error         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

CREATE TABLE dream_inputs (
    id          bigserial PRIMARY KEY,
    run_id      text NOT NULL REFERENCES dream_runs(id),
    source_type text NOT NULL,
    source_id   text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dream_summaries (
    id                  text PRIMARY KEY,
    run_id              text NOT NULL REFERENCES dream_runs(id),
    enterprise_id       text NOT NULL REFERENCES enterprises(id),
    space_id            text NOT NULL REFERENCES knowledge_spaces(id),
    layer               text NOT NULL CHECK (layer IN ('display','retrieval','sealed_pointer')),
    summary_text        text NOT NULL DEFAULT '' CHECK (char_length(summary_text) <= 4000),
    -- sealed detailed summaries live in object storage; only the key is here
    sealed_object_key   text NOT NULL DEFAULT '',
    evidence_pointer_id text REFERENCES evidence_pointers(id),
    risk_signals        jsonb NOT NULL DEFAULT '[]',
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dream_evidence_pointers (
    dream_summary_id    text NOT NULL REFERENCES dream_summaries(id),
    evidence_pointer_id text NOT NULL REFERENCES evidence_pointers(id),
    PRIMARY KEY (dream_summary_id, evidence_pointer_id)
);

-- ── work briefs and timeline ────────────────────────────────────────────────

CREATE TABLE work_briefs (
    id                  text PRIMARY KEY,
    enterprise_id       text NOT NULL REFERENCES enterprises(id),
    employee_user_id    text NOT NULL,
    brief_date          date NOT NULL,
    summary             text NOT NULL CHECK (char_length(summary) <= 2000),
    topics              text[] NOT NULL DEFAULT '{}',
    project_refs        text[] NOT NULL DEFAULT '{}',
    source_hash         text NOT NULL DEFAULT '',
    evidence_pointer_id text NOT NULL REFERENCES evidence_pointers(id),
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX work_briefs_source_hash_uniq
    ON work_briefs (enterprise_id, source_hash)
    WHERE source_hash <> '';

CREATE INDEX work_briefs_employee_date_idx
    ON work_briefs (enterprise_id, employee_user_id, brief_date DESC);

CREATE TABLE timeline_nodes (
    id                  text PRIMARY KEY,
    enterprise_id       text NOT NULL REFERENCES enterprises(id),
    space_id            text NOT NULL REFERENCES knowledge_spaces(id),
    org_scope           text NOT NULL DEFAULT '',
    node_time           timestamptz NOT NULL,
    source_type         text NOT NULL CHECK (source_type IN ('work_brief','dream_summary','sop_update','project_event','external_evidence','agent_answer')),
    summary_text        text NOT NULL CHECK (char_length(summary_text) <= 2000),
    tags                text[] NOT NULL DEFAULT '{}',
    evidence_pointer_id text REFERENCES evidence_pointers(id),
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX timeline_nodes_space_time_idx
    ON timeline_nodes (space_id, node_time DESC);

-- ── answer traces ───────────────────────────────────────────────────────────

CREATE TABLE answer_traces (
    id                         text PRIMARY KEY,
    enterprise_id              text NOT NULL REFERENCES enterprises(id),
    case_ticket_id             text NOT NULL,
    actor_user_id              text NOT NULL DEFAULT '',
    question_hash              text NOT NULL,
    sanitized_question_summary text NOT NULL DEFAULT '' CHECK (char_length(sanitized_question_summary) <= 1000),
    workflow_run_id            text REFERENCES workflow_runs(id),
    space_ids                  text[] NOT NULL DEFAULT '{}',
    retrieval_plan_id          text REFERENCES retrieval_plans(id),
    evidence_pointer_ids       text[] NOT NULL DEFAULT '{}',
    agentnexus_read_grant_ids  text[] NOT NULL DEFAULT '{}',
    model_route                text NOT NULL DEFAULT '',
    answer_hash                text NOT NULL,
    created_at                 timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE answer_trace_steps (
    id          bigserial PRIMARY KEY,
    trace_id    text NOT NULL REFERENCES answer_traces(id),
    step_no     integer NOT NULL,
    kind        text NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE answer_trace_evidence (
    trace_id            text NOT NULL REFERENCES answer_traces(id),
    evidence_pointer_id text NOT NULL REFERENCES evidence_pointers(id),
    grant_id            text NOT NULL DEFAULT '',
    PRIMARY KEY (trace_id, evidence_pointer_id)
);

CREATE TABLE answer_trace_model_events (
    id          bigserial PRIMARY KEY,
    trace_id    text NOT NULL REFERENCES answer_traces(id),
    model_route text NOT NULL,
    prompt_hash text NOT NULL DEFAULT '',
    usage       jsonb NOT NULL DEFAULT '{}',
    latency_ms  integer NOT NULL DEFAULT 0,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE answer_trace_audit_refs (
    trace_id     text NOT NULL REFERENCES answer_traces(id),
    audit_ref_id text NOT NULL,
    appended_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (trace_id, audit_ref_id)
);

-- +goose Down

DROP TABLE answer_trace_audit_refs;
DROP TABLE answer_trace_model_events;
DROP TABLE answer_trace_evidence;
DROP TABLE answer_trace_steps;
DROP TABLE answer_traces;
DROP TABLE timeline_nodes;
DROP TABLE work_briefs;
DROP TABLE dream_evidence_pointers;
DROP TABLE dream_summaries;
DROP TABLE dream_inputs;
DROP TABLE dream_runs;
DROP TABLE dream_policy_versions;
DROP TABLE dream_policies;
DROP TABLE index_documents;
DROP TABLE index_jobs;
DROP TABLE retrieval_results;
DROP TABLE retrieval_plan_steps;
DROP TABLE retrieval_plans;
DROP TABLE parser_provider_runs;
DROP TABLE document_summaries;
DROP TABLE document_blocks;
DROP TABLE atlas_documents;
DROP TABLE artifact_processing_steps;
DROP TABLE artifact_processing_jobs;
DROP TABLE artifacts;
DROP TABLE evidence_pointers;
DROP TABLE method_space_bindings;
DROP TABLE sop_workflow_bindings;
DROP TABLE method_outline_versions;
DROP TABLE method_outlines;
DROP TABLE sop_versions;
DROP TABLE sops;
DROP TABLE workflow_artifacts;
DROP TABLE workflow_run_events;
DROP TABLE workflow_runs;
DROP TABLE workflow_edges;
DROP TABLE workflow_nodes;
DROP TABLE workflow_versions;
DROP TABLE workflows;
DROP TABLE space_membership_cache;
DROP TABLE org_snapshots;
DROP TABLE org_scope_bindings;
DROP TABLE knowledge_space_versions;
DROP TABLE knowledge_spaces;
DROP TABLE enterprises;
