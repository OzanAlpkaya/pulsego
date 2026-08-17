-- Every other configurable field is bounded by a CHECK constraint; url was
-- the exception, and TEXT would accept a megabyte. The bound mirrors
-- domain.MaxURLLength.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE monitors
    ADD CONSTRAINT monitors_url_length CHECK (length(url) <= 2048);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE monitors
    DROP CONSTRAINT monitors_url_length;
-- +goose StatementEnd
