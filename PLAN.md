# NetMeta compose port: record

This replaces the earlier modernisation plan. NetMeta used to deploy to k3s with Bazel-built images and CUE
manifests. It now runs as a single-host Docker Compose stack. This file records what changed and why; the docs
describe how to run it.

## What changed

- **Deployment:** `compose.yaml` with `kafka` (KRaft), `clickhouse` (25.8 LTS), `migrate` (one-shot), `goflow2`
  (upstream v2, host network), `geoip` (optional), `grafana` (13), and `caddy` (profile `tls`). The settings are in
  `.env`; user data is in `config/data/*.tsv`. See [README.md](README.md).
- **Removed:**
  - Kubernetes/k3s, Strimzi, ZooKeeper, the clickhouse-operator and Traefik
  - Bazel (`MODULE.bazel`, `BUILD.bazel`, `build/`, `third_party/`) and CUE (`cue.mod/`, `deploy/`)
  - the reconciler, portmirror, FastNetMon, and risinfo, which `geoip` replaces
- **Schema:** static goose migrations in `sql/migrations`, functions in `sql/functions`, applied by `cmd/migrate`.
  - `flows_raw` (0001) is byte-identical to the old deployment's. `SrcCountry`/`DstCountry` are added after it (0004).
  - The ingest view maps goflow2 v2 field names explicitly. This fixes `EType` always being 0.
- **Enrichment:** optional and off by default (`GEOIP_PROVIDERS`). With it off, NetMeta makes no external calls.
  See [doc/enrichment.md](doc/enrichment.md).
- **Operations:** every ClickHouse system log has a TTL and `trace_log` is off. On the old host, unbounded system
  logs had filled the disk. See [doc/operations.md](doc/operations.md).
- **Build and CI:** a `Makefile`, `.github/workflows/ci.yml` and Dependabot.
  - CI runs vet, race tests, govulncheck, golangci-lint and the e2e test.
  - Pushes to main and `v*` tags build the `netmeta-geoip` and `netmeta-migrate` images for amd64/arm64 and push
    them to GHCR. Actions are pinned by SHA.

## Decisions

- **Compatibility with existing consumers:** the owner's Grafana instances and the "Sflow Overview" dashboard must
  keep working unmodified. So the following stay as they were:
  - ClickHouse HTTP on :15003 (plain HTTP, user `admin`)
  - the `flows_raw` columns and ORDER BY
  - the UDF and dictionary names
  - the datasource uid `b315efc1-b699-4ba8-a3a2-d6c0e711c5fd`

  `test/e2e.sh` checks this contract.
- **Ports are unchanged** (2055, 2056, 6343). goflow2 v2 has no `nfl` scheme, so 2056 is a second `netflow://`
  listener, which decodes v5 as well. See [doc/ingest.md](doc/ingest.md).
- **sFlow has no direction.** `isIncomingFlow` stays user-editable (`config/clickhouse/isIncomingFlow.sql`). Its
  default matches the old behaviour.
- **TLS is optional.** Caddy offers `internal` (self-signed, as before) or `acme`. Let's Encrypt only works when
  Caddy owns public :80 or :443.
- **Migration is read-only on the old host.** The data is copied through the old :15003 endpoint with HTTP GET
  `SELECT`s, and the old k3s deployment is not decommissioned. See [doc/migrate-from-k3s.md](doc/migrate-from-k3s.md)
  and `scripts/backfill.sh`.
