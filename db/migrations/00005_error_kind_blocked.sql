-- Adds the 'blocked' error kind: a check the SSRF policy (internal/security)
-- refused before connecting. Mirrored by internal/domain/check.go.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE monitor_checks
    DROP CONSTRAINT monitor_checks_error_kind_valid,
    ADD CONSTRAINT monitor_checks_error_kind_valid
        CHECK (error_kind IS NULL OR error_kind IN
            ('timeout', 'dns', 'connection', 'tls', 'redirect',
             'unexpected_status', 'blocked', 'unknown'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The old constraint does not know 'blocked'; keep those rows as 'unknown'
-- rather than deleting check history.
UPDATE monitor_checks SET error_kind = 'unknown' WHERE error_kind = 'blocked';

ALTER TABLE monitor_checks
    DROP CONSTRAINT monitor_checks_error_kind_valid,
    ADD CONSTRAINT monitor_checks_error_kind_valid
        CHECK (error_kind IS NULL OR error_kind IN
            ('timeout', 'dns', 'connection', 'tls', 'redirect',
             'unexpected_status', 'unknown'));
-- +goose StatementEnd
