# NetMeta modernisation plan

Goal: one host, `docker compose up`. NetMeta was designed to scale to many nodes, which was never finished, and the
project was abandoned. This plan strips that back to the minimum.

Target stack (5 long-running containers + 1 one-shot):

| Service       | Image                                   | Notes                                              |
|---------------|-----------------------------------------|----------------------------------------------------|
| `kafka`       | `apache/kafka:4.x` (KRaft, single node) | Buffer between UDP collectors and ClickHouse        |
| `clickhouse`  | `clickhouse/clickhouse-server:25.8`     | LTS; config.d/users.d/dictionaries mounted          |
| `migrate`     | goose (one-shot, `depends_on` healthy)  | Versioned SQL; replaces the reconciler loop         |
| `goflow2`     | `netsampler/goflow2` (upstream, pinned) | `network_mode: host` to keep sampler source IPs     |
| `risinfo`     | `ghcr.io/<owner>/netmeta-risinfo`       | Only custom Go service left                         |
| `grafana`     | `grafana/grafana:12.x`                  | Provisioned datasource + static dashboard JSON      |
| `caddy`       | `caddy:2` (profile `tls`, optional)     | Replaces Traefik                                    |

**Removed entirely:** Kubernetes/k3s, Strimzi, ZooKeeper, the Altinity clickhouse-operator, Traefik, the reconciler,
**portmirror**, Bazel, CUE, `third_party/`, `deploy/go.mod`.

User config becomes: `.env` (passwords, hostname, ports, retention) + `config/dictionaries/*.tsv`
(sampler/interface/VLAN/host names, own ASNs) + compose profiles.

---

## Phase 0: Prerequisites

- [ ] **Put the tree under git.** `/opt/slothnetworks/NetMeta` has no `.git`. Phase 4 deletes a lot of files. Either
      restore the real clone or `git init` and commit the current state as a baseline.
- [ ] Record the current CUE output before touching anything (needs `cue` v0.11 installed temporarily). This is the
      reference that phase 2 must reproduce:
      `cue export ./deploy/single-node/...`, `cue dump_dashboards`, plus the generated reconciler `config.json`
      (functions, Kafka tables, MVs).

## Phase 1: Remove portmirror

Delete:
- [ ] `cmd/portmirror/` (portmirror.go, iface.go, frame.go, BUILD.bazel)
- [ ] `deploy/single-node/k8s/portmirror/`
- [ ] `deploy/single-node/defs.cue:30` (import) and `:148-156` (conditional deployment)
- [ ] `deploy/single-node/config.cue:60` (`#PortMirrorConfig`), `:234-236` (`portMirror?` field), `:259` (image ref)
- [ ] `deploy/BUILD.bazel:16`, `deploy/single-node/BUILD.bazel:16` (image load targets)
- [ ] `doc/ingest.md:76` onward ("Port Mirror collector" section)
- [ ] `README.md:31` ("Port Mirror (AF_PACKET)")
- [ ] Portmirror sentences in `deploy/dashboards/NetMeta_Overview.cue:179,190` (interface ID 0 note)
- [ ] Portmirror wording in the `config_legacy.cue:4` comment (file is deleted in phase 4 anyway)

Then `go mod tidy`. This should drop `goflow2` v1, `gopacket`, `vishvananda/netlink`, `logrus`, `Shopify/sarama`
and `klauspost/compress` from `go.mod`.
- [ ] Verify: `go build ./... && go vet ./...` (also fixes the only current vet error, `iface.go:46`).

Docs: point users with a mirror port or tap at **hsflowd** (pcap module), which sends sFlow to goflow2.

This also removes 4 reachable vulnerabilities (see "Vulnerability scan" below).

## Phase 2: Docker Compose with the current wire format

Keep the protobuf contract unchanged in this phase, so any breakage is clearly the platform's fault.

- [ ] `deploy/compose/docker-compose.yml` with the services above, `restart: unless-stopped`, healthchecks
      (`/ping` for ClickHouse, `/api/health` for Grafana, `kafka-broker-api-versions.sh` for Kafka), named volumes.
      Bind Kafka and ClickHouse ports to `127.0.0.1` only.
- [ ] goflow2 **v1.3.7 upstream image** (confirm the tag exists on Docker Hub, else build v1.3.7 from a 5-line
      Dockerfile) with the existing args: `-format=pb -format.protobuf.fixedlen=true -transport=kafka`.
      Drop `-loglevel=debug`.
- [ ] ClickHouse config from the phase 0 export, rewritten as plain files:
      `config.d/*.xml` (use the `<clickhouse>` root, not `<yandex>`), `users.d/*.xml` with passwords via `from_env`
      and the `::/0` network ACL narrowed to the compose network.
      Mount `FlowMessage.proto` into `format_schemas/`. Strip the custom `(column_*)` options first, or add the missing
      `import "DBManager.proto"`, so ClickHouse 25.x can parse it.
- [ ] Migrations: turn the reconciler's output into goose files in `deploy/compose/sql/`:
      `0003_functions.sql` (`CREATE OR REPLACE FUNCTION`), `0004_kafka_tables.sql`, `0005_views.sql`,
      `0006_dictionaries.sql` (`CREATE DICTIONARY`, replacing the operator-inlined XML).
      Give the two Kafka tables **distinct** `kafka_group_name`s (both are `clickhouse` today).
- [ ] Replace Kubernetes DNS names: `netmeta-kafka-bootstrap:9092` becomes `kafka:9092`,
      `clickhouse-netmeta` becomes `clickhouse`, and `http://risinfo/` becomes `http://risinfo:8080/`.
- [ ] Grafana: `GF_PLUGINS_PREINSTALL=grafana-clickhouse-datasource@4.x,netsage-sankey-panel@1.1.3`.
      Provision the datasource with a fixed `uid` and the dashboards from the phase 0 JSON. Remove the dead
      "Local Prometheus" datasource and the `topnav` toggle. Set `cookie_secure` only when TLS is on.
      Turn `minInterval`/`maxPacketSize` into dashboard variables.
- [ ] `deploy/compose/.env.example` + `config/dictionaries/*.tsv` examples.
- [ ] Optional `caddy` service under the `tls` profile.
- [ ] Rewrite `deploy/single-node/README.md` as a compose quickstart. Delete `UPGRADE.md`'s k3s steps.

**Done when:** sFlow/NetFlow from a test sender (e.g. `nflow-generator`, or hsflowd on the host) shows up in
`flows_raw` and on the Overview dashboard, and the data survives `docker compose restart clickhouse`.

## Phase 3: Data-correctness fixes

These bugs are independent of the platform. Fix them in the new SQL files and the risinfo code:

- [ ] **Proto and table column names disagree:** `Etype`/`IngressVrfID`/`EgressVrfID` vs
      `EType`/`IngressVrfId`/`EgressVrfId`. The `SELECT *` view probably writes 0 to those columns.
      Make the names consistent, and give the view an explicit column list.
- [ ] **FastNetMon view anonymises `DstAddr` from `SrcAddr`** (`schema/views.cue:75`).
- [ ] **`isIncomingFlow` range handlers produce SQL that cannot run** (`schema/functions.cue:44-46`, misplaced
      parenthesis). The fix is `isIPAddressInRange(toString(X), toIPv6Net('…'))`.
- [ ] **A dictionary outage stops all ingestion.** In the ingest view, use `dictGetOrDefault` for `risinfo` and
      `SamplerConfig`.
- [ ] `ParseGoFlowAddress` treats IPv6 addresses whose low 96 bits are zero as IPv4. Check for the `::ffff:` prefix
      instead.
- [ ] `0002` Down migration: add `IF EXISTS`.
- [ ] Make the 1-week TTL configurable from `.env`.
- [ ] **risinfo hardening:**
  - replace `klog.Fatalf` in handlers with HTTP 500s
  - error out on "no dump found" (today it returns `nil, nil`, which panics)
  - close non-200 bodies
  - set client timeouts
  - bounds-check `attr.Value[0]`
  - refresh in the background and serve the stale cache (a miss currently blocks for about 60 s)
  - fetch from RouteViews/RIPE directly, not the original author's personal `netmeta-cache.leoluk.de` proxy
  - switch logging to `log/slog`

## Phase 4: goflow2 v2

Verified against `goflow2/v2@v2.2.1` `pb/flow.proto`:

| Field              | v1 (current)         | v2.2.1                             |
|--------------------|----------------------|------------------------------------|
| TimeReceived       | `2`, seconds         | `time_received_ns = 110`           |
| TimeFlowStart      | `38`, seconds        | `time_flow_start_ns = 111`; **38 is now `ip_flags`** |
| TimeFlowEnd        | `5`, seconds         | `time_flow_end_ns = 112`           |
| FlowDirection      | `42`                 | removed, but re-addable via mapping |
| Bi-flow dir / VRFs | `41` / `39`, `40`    | removed, but re-addable via mapping |

The upstream `cmd/goflow2/mapping.yaml` **already** re-adds 42/41/39/40 with the same numbers (IPFIX IE 61, 239,
234, 235). Ship a copy of it, and no fork or patch is needed.

- [ ] Rewrite `FlowMessage.proto` with the v2 numbering and add 39-42 as plain fields.
- [ ] In the ingest view: `TimeReceived = intDiv(time_received_ns, 1000000000)` (same for start/end), so
      `flows_raw` and the dashboards stay unchanged. Optionally migrate to `DateTime64(9)` later.
- [ ] goflow2 args: `-listen=sflow://:6343,netflow://:2055 -format=bin -transport=kafka
      -transport.kafka.brokers=kafka:9092 -transport.kafka.topic=flow-messages -mapping=/etc/goflow2/mapping.yaml
      -addr=:8080`.
- [ ] Cut over on a fresh Kafka topic so v1 and v2 messages never mix in one consumer group.
- [ ] Test before shipping: a single v2 flow must land with a correct timestamp. If the timestamps are wrong, rows
      are partitioned into 1970 and the TTL deletes them immediately.

## Phase 5: Delete the old build and deployment

- [ ] Bazel: `MODULE.bazel`, `MODULE.bazel.lock`, `.bazelrc`, `.bazelversion`, `.bazelproject`, `.aspect/`,
      every `BUILD.bazel`, `build/cue/`, `third_party/` (all 4 patch sets, 3 of which were never applied).
- [ ] Kubernetes/CUE: `deploy/k8s-base/`, `deploy/single-node/{k8s,schema/*.cue,tests,*_tool.cue,defs.cue,config*.cue}`,
      `cue.mod/`, `deploy/go.mod`, `deploy/go.sum`, `deploy/deps.go`.
- [ ] `cmd/reconciler/` (including the pointless `placeholder.s` and `quote.go`'s `_ "unsafe"`).
- [ ] `tools.go`, then `go mod tidy`. Only risinfo's dependencies should remain.
- [ ] `.github/workflows/build_images.yml` (it runs a `//:push` target that does not exist).
- [ ] `.gitignore`: drop `bazel-*`/`.ijwb` and add `.env`.
- [ ] Bump `go` in go.mod to the current release.

New:
- [ ] `Dockerfile` (multi-stage: `golang` builds `CGO_ENABLED=0 -trimpath` into `distroless/static:nonroot`) for risinfo.
- [ ] `Makefile`: `vet`, `test`, `lint`, `vuln`, `build`, `image`, `up`, `down`.
- [ ] `.github/workflows/ci.yml`. It runs on push and PR:
  - checks: `go vet`, `go test -race`, `govulncheck`, golangci-lint
  - on main and tags: buildx multi-arch (amd64/arm64) and push to GHCR
  - actions pinned by SHA, kept current by Dependabot

## Phase 6: Tests

- [ ] **Compose integration test (highest value):** bring up kafka, clickhouse and migrate, produce one goflow2
      protobuf message to Kafka, and assert the row in `flows_raw` (including `EType`, the timestamp and the dictionary
      enrichment). Run it in CI.
- [ ] Proto vs `flows_raw` column-name diff test. This catches the class of bug in phase 3.
- [ ] Run every dashboard `rawSql` against an empty schema (catches broken panels after a schema change).
- [ ] risinfo: `parseMRT` on a small MRT fixture, and handler tests for cache hit, miss and upstream failure.

---

## Vulnerability scan (govulncheck, 2026-11-27)

13 reachable vulnerabilities in 6 modules plus stdlib, and 44 more in required modules that aren't called.

| Module                 | IDs | Reached via            | Resolution                                   |
|------------------------|-----|------------------------|----------------------------------------------|
| `osrg/gobgp` v2.0.0+incompatible | GO-2026-4736, GO-2025-3630/3631/3632/3633 (DoS / panics in BGP parsing) | risinfo | **No fix on the v2 import path.** Port risinfo to `github.com/osrg/gobgp/v3` (`pkg/packet/{bgp,mrt}`). Highest priority: risinfo parses untrusted MRT data from the internet. |
| `gopacket/gopacket`    | GO-2026-6121, 6122, 6963 | portmirror | Removed with portmirror (phase 1) |
| `klauspost/compress`   | GO-2026-6865 | portmirror (sarama) | Removed with portmirror |
| `golang.org/x/text`    | GO-2026-5970 | portmirror | Removed with portmirror, or bump to ≥ v0.39.0 |
| `ClickHouse/ch-go`     | GO-2025-3603 | reconciler | Removed with the reconciler (phase 5) |
| `andybalholm/brotli`   | GO-2026-6888 | build/cue/local-images | Removed with Bazel (phase 5) |
| stdlib `net/http`      | GO-2026-6859 | all HTTP servers (risinfo) | Build with Go ≥ 1.27.4 |

After phases 1 and 5, only the gobgp issue and the stdlib issue remain, and both are fixed in risinfo.

## Suggested order

Phase 0, then 1, then 2, then 3, then 4, then 5, then 6. Phase 1 is small and safe, so it can be done right away.
Write the phase 6 integration test during phase 2 and use it as the gate for phases 3 and 4. Phase 5 is deliberately
late: the old tree is the reference until the compose stack has reproduced its behaviour.
