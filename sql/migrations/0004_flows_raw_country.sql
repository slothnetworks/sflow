-- ClickHouse has no DDL transactions.
-- +goose NO TRANSACTION

-- Country of source/destination address, from the optional geo_country dictionary ('' if unknown).
-- Added after the existing columns so 0001 stays identical to the old deployment's flows_raw.

-- +goose Up
ALTER TABLE flows_raw
    ADD COLUMN IF NOT EXISTS SrcCountry LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS DstCountry LowCardinality(String) DEFAULT '';

-- +goose Down
ALTER TABLE flows_raw
    DROP COLUMN IF EXISTS SrcCountry,
    DROP COLUMN IF EXISTS DstCountry;
