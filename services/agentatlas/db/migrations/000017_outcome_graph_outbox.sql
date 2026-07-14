-- +goose Up

-- outcome_graph_outbox is the ONE shared, authoritative, append-only projection
-- outbox for the Task 0I Outcome Graph. All FOUR authoritative stores (workcase,
-- operatingmap, governance, outcome) append a canonical projection event into
-- THIS table WITHIN THE SAME PostgreSQL transaction as their own domain write
-- (internal/outcomegraph.AppendOutboxTx). The only synchronous write is to this
-- authoritative database (domain row + outbox row, atomically); Apache AGE is
-- updated ASYNCHRONOUSLY by the projector reading this table. There is NO
-- PostgreSQL/AGE dual write.
--
-- Reconciliation with Task 0G: this table SUPERSEDES the 0G-era, outcome-only
-- outcome_projection_events table (migration 000016). The outcome store's
-- AppendProjectionEvent is re-pointed onto THIS shared outbox so there is exactly
-- ONE physical outbox for every domain — no two competing outboxes can diverge.
-- (000016's outcome_projection_events is retired: it is no longer written. Its
-- outcome_projection_watermarks table is retained only for the legacy 0G Store
-- watermark API; the authoritative Outcome-Graph projection watermark is owned by
-- the graph provider and advanced only after the AGE write commits.)
--
-- payload is the canonical GraphDelta (internal/outcomegraph.GraphDelta): the
-- exact set of versioned nodes/edges to MERGE (upsert) or the target node to
-- flag (tombstone/reevaluation). It carries ONLY opaque, classified data —
-- versioned business ids, bounded permitted summaries and opaque hashes — never
-- raw enterprise content, a connector endpoint, a credential or a full receipt.
-- sequence is per-tenant monotonic and assigned under a transaction-scoped
-- advisory lock; (tenant, sequence) is the primary key. Events are IMMUTABLE:
-- a source revocation/supersession APPENDS a tombstone/reevaluation naming the
-- sequence it supersedes; it never edits or prunes an existing event.
CREATE TABLE outcome_graph_outbox (
    tenant              text   NOT NULL REFERENCES enterprises(id),
    sequence            bigint NOT NULL CHECK (sequence >= 1),
    org                 text,
    source              text   NOT NULL CHECK (source IN ('workcase','operatingmap','governance','outcome')),
    kind                text   NOT NULL CHECK (kind IN ('upsert','tombstone','reevaluation')),
    subject_label       text   NOT NULL CHECK (subject_label IN (
                            'Outcome','Goal','WorkCase','Observation','ActionReceipt',
                            'Blocker','Contributor','Evidence','OperatingMap','GovernanceChange')),
    subject_id          text   NOT NULL CHECK (subject_id <> ''),
    subject_revision    bigint NOT NULL CHECK (subject_revision >= 1),
    payload             jsonb  NOT NULL,
    payload_hash        text   NOT NULL CHECK (payload_hash <> ''),
    supersedes_sequence bigint,
    recorded_at         timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, sequence)
);

-- The projector reads events per tenant in ascending sequence order; the PK's
-- btree already serves that scan.

-- Append-only: the projection outbox is an authoritative rebuild input and must
-- never be rewritten, deleted or truncated in place (mirrors the 000016 immutable
-- guards). Row-level triggers do not fire on TRUNCATE, so a statement-level guard
-- is added too.
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_outcome_graph_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is an append-only projection outbox (immutable)', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outcome_graph_outbox_immutable
BEFORE UPDATE OR DELETE ON outcome_graph_outbox
FOR EACH ROW EXECUTE FUNCTION reject_immutable_outcome_graph_change();
CREATE TRIGGER outcome_graph_outbox_no_truncate
BEFORE TRUNCATE ON outcome_graph_outbox
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_outcome_graph_change();

-- +goose Down

-- Defensive guard mirroring 000016: the projection outbox is an authoritative
-- append-only rebuild input, so refuse to drop it silently once history exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM outcome_graph_outbox) THEN
        RAISE EXCEPTION 'cannot downgrade 000017: outcome_graph_outbox projection history exists';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER outcome_graph_outbox_no_truncate ON outcome_graph_outbox;
DROP TRIGGER outcome_graph_outbox_immutable ON outcome_graph_outbox;
DROP FUNCTION reject_immutable_outcome_graph_change();
DROP TABLE outcome_graph_outbox;
