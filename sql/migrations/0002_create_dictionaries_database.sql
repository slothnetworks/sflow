-- ClickHouse has no DDL transactions.
-- +goose NO TRANSACTION

-- +goose Up
CREATE DATABASE IF NOT EXISTS dictionaries Engine=Dictionary;

-- +goose Down
DROP DATABASE IF EXISTS dictionaries;