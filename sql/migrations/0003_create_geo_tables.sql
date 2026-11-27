-- ClickHouse has no DDL transactions.
-- +goose NO TRANSACTION

-- Optional ASN/country enrichment data, written by the geoip loader (cmd/geoip) and read by the
-- geo_asn, geo_country and autnums dictionaries. Empty unless GEOIP_PROVIDERS is set.
-- `network` is a CIDR string; IPv4 networks may be plain ('10.0.0.0/8') or IPv4-mapped
-- ('::ffff:10.0.0.0/104'); the ip_trie dictionaries accept both.

-- +goose Up
CREATE DATABASE IF NOT EXISTS geo;
CREATE TABLE IF NOT EXISTS geo.asn
(
    network String,
    asn     UInt32
) ENGINE = MergeTree ORDER BY tuple();
CREATE TABLE IF NOT EXISTS geo.country
(
    network String,
    country LowCardinality(String)
) ENGINE = MergeTree ORDER BY tuple();
CREATE TABLE IF NOT EXISTS geo.asn_names
(
    asn     UInt32,
    name    String,
    country String
) ENGINE = MergeTree ORDER BY tuple();

-- +goose Down
DROP DATABASE IF EXISTS geo;
