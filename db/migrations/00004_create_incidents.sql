-- +goose Up
-- +goose StatementBegin
CREATE TABLE incidents (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    monitor_id       UUID        NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    started_at       TIMESTAMPTZ NOT NULL,
    resolved_at      TIMESTAMPTZ,
    opening_check_id BIGINT      REFERENCES monitor_checks (id) ON DELETE SET NULL,
    closing_check_id BIGINT      REFERENCES monitor_checks (id) ON DELETE SET NULL,
    cause            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incidents_resolved_after_start
        CHECK (resolved_at IS NULL OR resolved_at >= started_at),
    CONSTRAINT incidents_closing_only_when_resolved
        CHECK (closing_check_id IS NULL OR resolved_at IS NOT NULL),
    CONSTRAINT incidents_cause_length
        CHECK (cause IS NULL OR length(cause) <= 512)
);
-- +goose StatementEnd

-- DoD 3: the database, not the application, guarantees one open incident.
-- +goose StatementBegin
CREATE UNIQUE INDEX incidents_one_unresolved_per_monitor
    ON incidents (monitor_id) WHERE resolved_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX incidents_history_idx
    ON incidents (monitor_id, started_at DESC, id DESC);
-- +goose StatementEnd

-- Keep retention's ON DELETE SET NULL from scanning incidents per deleted check.
-- +goose StatementBegin
CREATE INDEX incidents_opening_check_idx
    ON incidents (opening_check_id) WHERE opening_check_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX incidents_closing_check_idx
    ON incidents (closing_check_id) WHERE closing_check_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE incidents;
-- +goose StatementEnd
