-- +goose Up
ALTER TABLE atlas_browser_logout_operations
    ADD COLUMN quarantined_at timestamptz,
    ADD COLUMN quarantine_reason text,
    ADD CONSTRAINT atlas_browser_logout_operations_quarantine_check CHECK (
        (quarantined_at IS NULL AND quarantine_reason IS NULL)
        OR (quarantined_at IS NOT NULL AND quarantine_reason IN ('credential_decrypt_failed'))
    );

CREATE INDEX atlas_browser_logout_operations_pending_idx
    ON atlas_browser_logout_operations(created_at)
    WHERE quarantined_at IS NULL;

-- +goose Down
DROP INDEX atlas_browser_logout_operations_pending_idx;
ALTER TABLE atlas_browser_logout_operations
    DROP CONSTRAINT atlas_browser_logout_operations_quarantine_check,
    DROP COLUMN quarantine_reason,
    DROP COLUMN quarantined_at;
