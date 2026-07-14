-- +goose Up

-- outcome_learning_candidates is the append-only, IMMUTABLE store of GOVERNED
-- LEARNING CANDIDATES distilled by the Task 0J outcome-learning service
-- (internal/outcomelearning) from the provable Outcome lineage (0G/0H) as
-- projected by the 0I Outcome Graph.
--
-- Every row is an immutable governed candidate. It is NEVER mutated in place: a
-- re-evaluation (e.g. after a source Outcome is revoked or superseded) APPENDS a
-- NEW candidate that supersedes the prior one (supersedes_id), mirroring the
-- append-only Outcome and projection-outbox discipline (migrations 000016/000017).
--
-- A candidate is NOT published knowledge/method/Operating-Map/risk/assessment
-- policy. An ACCEPTED candidate is handed to the EXISTING governance draft path
-- (governance_change_id references the resulting suggestion-only ChangeDraft,
-- which only a human/governed review can adopt); a QUARANTINED candidate is
-- recorded here but never reaches governance. This table therefore holds only
-- opaque, classified metadata: versioned business ids, bounded permitted
-- summaries, opaque source references, coverage/replay/shadow verdicts and the
-- deterministic candidate id — never raw enterprise content, a connector
-- endpoint, a credential or a full receipt.
--
-- id is the deterministic, provider-neutral candidate id
-- (internal/outcomelearning.Candidate.deriveID); (tenant, id) is the primary key,
-- so re-distilling identical evidence is idempotent.
CREATE TABLE outcome_learning_candidates (
    tenant              text   NOT NULL REFERENCES enterprises(id),
    id                  text   NOT NULL CHECK (id <> ''),
    org                 text,
    kind                text   NOT NULL CHECK (kind IN (
                            'method','operating_map_change','sop_change','risk_rule',
                            'connector_capability_gap','data_authority_conflict','recurring_blocker_pattern')),
    summary             text   NOT NULL DEFAULT '',
    watermark           bigint NOT NULL CHECK (watermark >= 1),
    anchor              jsonb  NOT NULL,
    sources             jsonb  NOT NULL,
    generation_policy   text   NOT NULL CHECK (generation_policy <> ''),
    model_version       text   NOT NULL CHECK (model_version <> ''),
    coverage            jsonb  NOT NULL,
    replay              jsonb  NOT NULL,
    shadow              jsonb  NOT NULL,
    proposal            jsonb  NOT NULL,
    disposition         text   NOT NULL CHECK (disposition IN ('accepted','quarantined')),
    quarantine_reason   text   NOT NULL DEFAULT '',
    governance_change_id text  NOT NULL DEFAULT '',
    supersedes_id       text   NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    recorded_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, id),

    -- The no-silent-publication invariant, enforced in the schema itself:
    --   * an ACCEPTED candidate carries no quarantine reason and MAY carry the
    --     governance draft id it was handed to (an accepted candidate reaches
    --     ONLY the draft path, never publish);
    --   * a QUARANTINED candidate MUST name its reason and MUST NOT carry any
    --     governance handoff (it never reaches governance).
    CONSTRAINT outcome_learning_disposition_coherent CHECK (
        (disposition = 'accepted'   AND quarantine_reason = '')
        OR
        (disposition = 'quarantined' AND quarantine_reason <> '' AND governance_change_id = '')
    )
);

-- The service reads a tenant's candidates and re-evaluation chains by supersedes.
CREATE INDEX outcome_learning_candidates_tenant_kind ON outcome_learning_candidates (tenant, kind);
CREATE INDEX outcome_learning_candidates_supersedes ON outcome_learning_candidates (tenant, supersedes_id)
    WHERE supersedes_id <> '';

-- Append-only immutability: a governed candidate is an audit record of what the
-- learning layer proposed from which evidence at which watermark, so it must
-- never be rewritten, deleted or truncated in place (mirrors the 000016/000017
-- immutable guards). Row-level triggers do not fire on TRUNCATE, so a
-- statement-level guard is added too.
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_outcome_learning_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is an append-only governed-candidate store (immutable); re-evaluations append a new superseding candidate', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outcome_learning_candidates_immutable
BEFORE UPDATE OR DELETE ON outcome_learning_candidates
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_learning_change();
CREATE TRIGGER outcome_learning_candidates_no_truncate
BEFORE TRUNCATE ON outcome_learning_candidates
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_learning_change();

-- +goose Down

-- Defensive guard mirroring 000016/000017: the governed-candidate store is an
-- append-only audit record, so refuse to drop it silently once history exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM outcome_learning_candidates) THEN
        RAISE EXCEPTION 'cannot downgrade 000018: outcome_learning_candidates history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER outcome_learning_candidates_no_truncate ON outcome_learning_candidates;
DROP TRIGGER outcome_learning_candidates_immutable ON outcome_learning_candidates;
DROP FUNCTION reject_immutable_outcome_learning_change();
DROP INDEX outcome_learning_candidates_supersedes;
DROP INDEX outcome_learning_candidates_tenant_kind;
DROP TABLE outcome_learning_candidates;
