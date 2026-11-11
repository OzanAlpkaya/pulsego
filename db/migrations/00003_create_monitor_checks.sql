-- +goose Up
-- +goose StatementBegin
CREATE TABLE monitor_checks (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    monitor_id    UUID        NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    success       BOOLEAN     NOT NULL,
    status_code   INT,
    latency_ms    INT         NOT NULL,
    error_kind    VARCHAR(32),
    error_message TEXT,
    checked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Bounds mirrored by internal/domain/check.go.
    CONSTRAINT monitor_checks_status_code_range
        CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
    CONSTRAINT monitor_checks_latency_non_negative
        CHECK (latency_ms >= 0),
    CONSTRAINT monitor_checks_error_kind_valid
        CHECK (error_kind IS NULL OR error_kind IN
            ('timeout', 'dns', 'connection', 'tls', 'redirect',
             'unexpected_status', 'unknown')),
    CONSTRAINT monitor_checks_error_message_length
        CHECK (error_message IS NULL OR length(error_message) <= 512),
    -- A check either succeeded with no error, or failed with a kind.
    CONSTRAINT monitor_checks_outcome_consistent
        CHECK (success = (error_kind IS NULL)),
    CONSTRAINT monitor_checks_message_only_on_failure
        CHECK (error_message IS NULL OR NOT success),
    CONSTRAINT monitor_checks_success_has_status
        CHECK (NOT success OR status_code IS NOT NULL),
    CONSTRAINT monitor_checks_unexpected_status_has_status
        CHECK (error_kind IS DISTINCT FROM 'unexpected_status'
               OR status_code IS NOT NULL)
);
-- +goose StatementEnd

-- History endpoint (keyset over checked_at DESC, id DESC).
-- +goose StatementBegin
CREATE INDEX monitor_checks_history_idx
    ON monitor_checks (monitor_id, checked_at DESC, id DESC);
-- +goose StatementEnd

-- Retention cleanup (Sprint 4).
-- +goose StatementBegin
CREATE INDEX monitor_checks_checked_at_idx ON monitor_checks (checked_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE monitor_checks;
-- +goose StatementEnd
