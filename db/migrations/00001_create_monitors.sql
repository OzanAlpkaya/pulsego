-- +goose Up
-- +goose StatementBegin
CREATE TABLE monitors (
    id                   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name                 VARCHAR(120) NOT NULL,
    url                  TEXT         NOT NULL,

    interval_seconds     INT          NOT NULL DEFAULT 60,
    timeout_ms           INT          NOT NULL DEFAULT 5000,
    expected_status_min  SMALLINT     NOT NULL DEFAULT 200,
    expected_status_max  SMALLINT     NOT NULL DEFAULT 399,
    failure_threshold    SMALLINT     NOT NULL DEFAULT 3,
    enabled              BOOLEAN      NOT NULL DEFAULT TRUE,

    state                VARCHAR(16)  NOT NULL DEFAULT 'unknown',
    consecutive_failures INT          NOT NULL DEFAULT 0,
    last_checked_at      TIMESTAMPTZ,
    last_status_code     INT,
    last_latency_ms      INT,

    next_check_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    lease_owner          TEXT,
    lease_until          TIMESTAMPTZ,

    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at           TIMESTAMPTZ,

    -- Bounds mirrored by the constants in internal/domain/monitor.go.
    CONSTRAINT monitors_name_not_blank
        CHECK (length(btrim(name)) > 0),
    CONSTRAINT monitors_url_not_blank
        CHECK (length(btrim(url)) > 0),
    CONSTRAINT monitors_interval_range
        CHECK (interval_seconds BETWEEN 10 AND 86400),
    CONSTRAINT monitors_timeout_range
        CHECK (timeout_ms BETWEEN 100 AND 60000),
    CONSTRAINT monitors_expected_status_min_range
        CHECK (expected_status_min BETWEEN 100 AND 599),
    CONSTRAINT monitors_expected_status_max_range
        CHECK (expected_status_max BETWEEN 100 AND 599),
    CONSTRAINT monitors_expected_status_order
        CHECK (expected_status_min <= expected_status_max),
    CONSTRAINT monitors_failure_threshold_range
        CHECK (failure_threshold BETWEEN 1 AND 10),
    CONSTRAINT monitors_consecutive_failures_non_negative
        CHECK (consecutive_failures >= 0),
    CONSTRAINT monitors_state_valid
        CHECK (state IN ('unknown', 'up', 'degraded', 'down')),
    CONSTRAINT monitors_last_status_code_range
        CHECK (last_status_code IS NULL OR last_status_code BETWEEN 100 AND 599),
    CONSTRAINT monitors_last_latency_non_negative
        CHECK (last_latency_ms IS NULL OR last_latency_ms >= 0),
    -- A lease is meaningful only as an (owner, expiry) pair.
    CONSTRAINT monitors_lease_paired
        CHECK ((lease_owner IS NULL) = (lease_until IS NULL))
);
-- +goose StatementEnd

-- The scheduler's only hot query: due, enabled, not soft-deleted.
-- +goose StatementBegin
CREATE INDEX monitors_due_idx
    ON monitors (enabled, next_check_at)
    WHERE deleted_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE monitors;
-- +goose StatementEnd
