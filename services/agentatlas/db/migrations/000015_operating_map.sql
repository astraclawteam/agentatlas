-- +goose Up

-- Operating Map entries are enterprise cognition of WHERE and HOW to work
-- for one business intent: business semantics only (data needs, business
-- capabilities, correlation/authority/freshness/conflict rules), bound to
-- enterprise governance (org scope, org version, effective interval,
-- source policy revision, governance review) and published as immutable,
-- append-only versions. content never carries a connector endpoint,
-- credential or raw request body — sdk/go/operatingmap.NoConnectorLeak
-- enforces that boundary on every entry before it reaches Publish.
CREATE TABLE operating_map_entries (
    id                     text PRIMARY KEY,
    enterprise_id          text NOT NULL REFERENCES enterprises(id),
    org_scope              text NOT NULL CHECK (org_scope <> ''),
    org_version            bigint NOT NULL CHECK (org_version >= 0),
    intent_key             text NOT NULL CHECK (intent_key ~ '^[a-z0-9_]+(\.[a-z0-9_]+)*$'),
    version                integer NOT NULL CHECK (version > 0),
    effective_from         timestamptz NOT NULL,
    effective_to           timestamptz,
    source_policy_revision text NOT NULL CHECK (source_policy_revision <> ''),
    governance_review_ref  text NOT NULL CHECK (governance_review_ref <> ''),
    -- content holds intent_phrases, data_needs, method_refs,
    -- business_capabilities, correlation_rules, authority_rules,
    -- freshness_rules, conflict_rules and memory_refs (see
    -- internal/operatingmap/postgres.go's entryContent). The two CHECKs
    -- below are a defense-in-depth mirror of
    -- sdk/go/operatingmap.Entry.Validate's same two invariants; the Go
    -- validation is authoritative, this is a second, independent gate.
    content                jsonb NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enterprise_id, org_scope, intent_key, version),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    CHECK (jsonb_typeof(content->'data_needs') = 'array' AND jsonb_array_length(content->'data_needs') > 0),
    CHECK (jsonb_typeof(content->'intent_phrases') = 'array' AND jsonb_array_length(content->'intent_phrases') > 0)
);

CREATE INDEX operating_map_entries_scope_interval_idx
    ON operating_map_entries (enterprise_id, org_scope, effective_from, effective_to);

-- Published entries are historical governance records: insertable once,
-- never rewritten or removed in place (mirrors
-- reject_immutable_dream_output_change in migration 000007, scoped to this
-- table's own domain rather than reusing Dream's function).
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_operating_map_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable and append-only', TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER operating_map_entries_immutable
BEFORE UPDATE OR DELETE ON operating_map_entries
FOR EACH ROW EXECUTE FUNCTION reject_immutable_operating_map_change();

-- Row-level triggers do not fire on TRUNCATE, so append-only also needs a
-- statement-level guard or the whole history could be wiped in one
-- statement the row trigger never sees.
CREATE TRIGGER operating_map_entries_no_truncate
BEFORE TRUNCATE ON operating_map_entries
FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_operating_map_change();

-- +goose Down

-- Defensive guard mirroring 000013_dream_policy_adoptions.sql: published
-- Operating Map entries are historical governance records, so refuse to
-- drop them silently on downgrade.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM operating_map_entries) THEN
        RAISE EXCEPTION 'cannot downgrade 000015: published Operating Map entries exist';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER operating_map_entries_no_truncate ON operating_map_entries;
DROP TRIGGER operating_map_entries_immutable ON operating_map_entries;
DROP FUNCTION reject_immutable_operating_map_change();
DROP TABLE operating_map_entries;
