#!/usr/bin/env bash
# End-to-end test for the NetMeta compose stack.
#
#   test/e2e.sh          bring the stack up, test it, tear it down (docker compose down -v)
#   test/e2e.sh --keep   leave the stack running afterwards
#   test/e2e.sh --with-geoip   use GEOIP_PROVIDERS from .env (default: forced empty)
#
# Steps:
#   1. create .env with random passwords if there is none
#   2. docker compose up -d --build --wait
#   3. send ~20 s of NetFlow v5 (networkstatic/nflow-generator) to 127.0.0.1:$NETFLOW_PORT
#   4. assert flows land in flows_raw with sane timestamps, EType set, no enrichment
#   5. run every query from every dashboard in config/grafana/dashboards as the grafana user
#   6. check the compatibility contract (UDF and dictionary names)
#   7. optional enrichment path: hand-written geo.asn / geo.country rows must be applied
#   8. docker compose restart clickhouse: ingestion must resume
#
# Makes no external network calls (GEOIP_PROVIDERS is forced empty), image pulls aside.
# An existing .env is used as is and never modified.
# If a `netmeta` stack is already running when the test starts, it is left running and its
# volumes are kept (the test then adds a few hundred synthetic flows to it).
set -euo pipefail

KEEP=0
WITH_GEOIP=0
for arg in "$@"; do
	case "$arg" in
	--keep) KEEP=1 ;;
	--with-geoip) WITH_GEOIP=1 ;;
	-h | --help)
		sed -n '2,20p' "$0"
		exit 0
		;;
	*)
		echo "unknown argument: $arg" >&2
		exit 2
		;;
	esac
done

cd "$(dirname "$0")/.."
ROOT=$(pwd)
COMPOSE=(docker compose -p netmeta)
GENERATOR_IMAGE=networkstatic/nflow-generator
GENERATOR_NAME=netmeta-e2e-nflow-generator

log() { printf '\n==> %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}
pass() { printf 'ok   %s\n' "$*"; }

# --- 1. .env ----------------------------------------------------------------
randhex() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
if [[ ! -f .env ]]; then
	log "creating .env with random passwords"
	sed -e "s/^CLICKHOUSE_PASSWORD=$/CLICKHOUSE_PASSWORD=$(randhex)/" \
		-e "s/^CLICKHOUSE_GRAFANA_PASSWORD=$/CLICKHOUSE_GRAFANA_PASSWORD=$(randhex)/" \
		-e "s/^GRAFANA_ADMIN_PASSWORD=$/GRAFANA_ADMIN_PASSWORD=$(randhex)/" \
		-e "s/^GRAFANA_SECRET_KEY=$/GRAFANA_SECRET_KEY=$(randhex)/" \
		.env.example >.env
fi
set -a
# shellcheck disable=SC1091
source .env
set +a
# The test makes no external calls: the geoip loader runs with no providers, whatever .env says.
# (Exported, so it overrides .env for docker compose too. --with-geoip keeps the .env value.)
if ((WITH_GEOIP == 0)); then
	export GEOIP_PROVIDERS=
fi
CH_PORT=${CLICKHOUSE_HTTP_PORT:-15003}
NF_PORT=${NETFLOW_PORT:-2055}
CH_URL="http://127.0.0.1:${CH_PORT}/"

# --- teardown -------------------------------------------------------------------
PREEXISTING=0
if [[ -n "$("${COMPOSE[@]}" ps -q 2>/dev/null)" ]]; then
	PREEXISTING=1
	echo "note: a netmeta stack is already running; it will be left running"
fi
cleanup() {
	local rc=$?
	docker rm -f "$GENERATOR_NAME" >/dev/null 2>&1 || true
	if ((rc != 0)); then
		echo "--- service state and recent logs (exit $rc)" >&2
		"${COMPOSE[@]}" ps -a >&2 || true
		"${COMPOSE[@]}" logs --tail=40 >&2 || true
	fi
	if ((KEEP == 0 && PREEXISTING == 0)); then
		log "tearing down"
		"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
	else
		echo "stack left running (docker compose -p netmeta down -v to remove it)"
	fi
	exit $rc
}
trap cleanup EXIT

# ch QUERY [user password]: run a query over ClickHouse HTTP, print the result, fail on error.
ch() {
	curl -sS --fail-with-body --max-time 60 \
		-H "X-ClickHouse-User: ${2:-${CLICKHOUSE_USER:-admin}}" \
		-H "X-ClickHouse-Key: ${3:-$CLICKHOUSE_PASSWORD}" \
		--data-binary "$1" "$CH_URL"
}

# send_flows SECONDS: NetFlow v5 from the generator to goflow2.
send_flows() {
	docker rm -f "$GENERATOR_NAME" >/dev/null 2>&1 || true
	docker run -d --name "$GENERATOR_NAME" --network host "$GENERATOR_IMAGE" \
		-t 127.0.0.1 -p "$NF_PORT" >/dev/null
	sleep "$1"
	docker rm -f "$GENERATOR_NAME" >/dev/null
}

# wait_rows SQL_CONDITION: wait up to 90 s for flows_raw rows matching the condition.
wait_rows() {
	local n=0
	for _ in $(seq 1 45); do
		n=$(ch "SELECT count() FROM flows_raw WHERE $1")
		((n > 0)) && {
			echo "$n"
			return 0
		}
		sleep 2
	done
	echo 0
}

# synthetic_flow SEQ SRC_AS: produce one IPFIX message (IPv4 source, IPv6 destination) through
# the Kafka table, so the address parsing and enrichment of the view are tested with known input.
synthetic_flow() {
	ch "INSERT INTO flows_queue (type, time_received_ns, sequence_num, sampler_address, src_addr,
		dst_addr, etype, proto, src_port, dst_port, bytes, packets, src_as, dst_as, flow_direction)
		SELECT 'IPFIX', toUnixTimestamp64Nano(now64(9)), $1, unhex('C0000201'), unhex('C6336401'),
		unhex('20010DB8000000000000000000000001'), 34525, 6, 443, 50000, 1500, 1, $2, 0, 1"
}
# flow_fields SEQ: the parsed fields of the synthetic flow, once it has arrived.
flow_fields() {
	local q="SELECT toString(SamplerAddress), toString(SrcAddr), toString(DstAddr), EType,
		FlowDirection, SrcAS, DstAS, SrcCountry, DstCountry FROM flows_raw
		WHERE FlowType = 'IPFIX' AND SequenceNum = $1 FORMAT TSV"
	for _ in $(seq 1 45); do
		out=$(ch "$q")
		[[ -n "$out" ]] && {
			echo "$out" | tr '\t' ' '
			return 0
		}
		sleep 2
	done
	return 1
}

# --- 2. up ----------------------------------------------------------------------
log "docker compose up -d --build --wait"
docker pull -q "$GENERATOR_IMAGE" >/dev/null &
pull_pid=$!
"${COMPOSE[@]}" up -d --build --wait --wait-timeout 600
wait "$pull_pid" || fail "could not pull $GENERATOR_IMAGE"
"${COMPOSE[@]}" ps --format 'table {{.Service}}\t{{.Status}}'
migrate_state=$("${COMPOSE[@]}" ps -a migrate --format '{{.State}} {{.ExitCode}}')
[[ "$migrate_state" == "exited 0" ]] || fail "migrate did not complete: $migrate_state"
pass "migrate completed"

# --- 3/4. ingest --------------------------------------------------------------
T0=$(ch "SELECT toUnixTimestamp(now())")
log "sending NetFlow to 127.0.0.1:${NF_PORT} for 20 s"
send_flows 20
n=$(wait_rows "TimeReceived >= $T0 - 5")
((n > 0)) || fail "no flows arrived in flows_raw"
pass "flows_raw: $n new rows"

read -r total bad_time with_etype with_country <<<"$(ch "
	SELECT count(),
	       countIf(abs(toInt64(TimeReceived) - toInt64(toUnixTimestamp(now()))) > 300),
	       countIf(EType != 0),
	       countIf(SrcCountry != '' OR DstCountry != '')
	FROM flows_raw WHERE TimeReceived >= $T0 - 5 FORMAT TSV")"
((bad_time == 0)) || fail "$bad_time of $total rows have TimeReceived more than 5 min from now"
pass "TimeReceived within +-5 min of now"
((with_etype > 0)) || fail "EType is 0 on every row"
pass "EType set on $with_etype/$total rows"
if [[ -z "${GEOIP_PROVIDERS:-}" ]]; then
	((with_country == 0)) || fail "$with_country rows have a country although GEOIP_PROVIDERS is empty"
	pass "no enrichment without GEOIP_PROVIDERS (SrcCountry/DstCountry empty)"
fi

SEQ=$((RANDOM * 32768 + RANDOM))
synthetic_flow "$SEQ" 0
got=$(flow_fields "$SEQ") || fail "synthetic IPFIX flow (IPv6 destination) did not arrive"
want="::ffff:192.0.2.1 ::ffff:198.51.100.1 2001:db8::1 34525 1 0 0  "
[[ "$got" == "$want" ]] || fail "synthetic flow parsed as '$got', want '$want'"
pass "synthetic IPFIX flow parsed: $got"

# --- 5. dashboard queries -------------------------------------------------------
log "running dashboard queries as the grafana user"
python3 -I "$ROOT/test/dashboard_queries.py" --url "$CH_URL" --user grafana \
	--password "$CLICKHOUSE_GRAFANA_PASSWORD" "$ROOT"/config/grafana/dashboards/*.json

log "querying through the bundled Grafana"
G="http://127.0.0.1:${GRAFANA_HTTP_PORT:-15001}"
DS_UID=b315efc1-b699-4ba8-a3a2-d6c0e711c5fd
curl -sS --fail-with-body -u "admin:$GRAFANA_ADMIN_PASSWORD" "$G/api/datasources/uid/$DS_UID/health" |
	grep -q '"status":"OK"' || fail "Grafana's ClickHouse datasource is not healthy"
for uid in 9Dw5dGzGk; do
	curl -sS --fail-with-body -o /dev/null -u "admin:$GRAFANA_ADMIN_PASSWORD" "$G/api/dashboards/uid/$uid" ||
		fail "dashboard $uid is not provisioned"
done
resp=$(curl -sS --fail-with-body -u "admin:$GRAFANA_ADMIN_PASSWORD" -H 'Content-Type: application/json' \
	"$G/api/ds/query" -d '{"from":"now-1h","to":"now","queries":[{"refId":"A",
	"datasource":{"uid":"'$DS_UID'","type":"grafana-clickhouse-datasource"},"format":1,"editorType":"sql",
	"rawSql":"SELECT $__timeInterval(TimeReceived) AS t, sum(Bytes) FROM flows_raw WHERE $__timeFilter(TimeReceived) GROUP BY t"}]}') ||
	fail "Grafana query failed: $resp"
grep -q '"status":200' <<<"$resp" || fail "Grafana query failed: ${resp:0:300}"
pass "datasource uid $DS_UID healthy, Sflow Overview (9Dw5dGzGk) provisioned, query OK"

# --- 6. compatibility contract ------------------------------------------------
log "compatibility contract"
for f in HostToString InterfaceToString SamplerToString VLANToString isIncomingFlow \
	toIPv6Net IPv6ToString ASNToString ParseGoFlowAddress IPv6CIDRMatch; do
	[[ $(ch "SELECT count() FROM system.functions WHERE name = '$f' AND origin = 'SQLUserDefined'") == 1 ]] ||
		fail "UDF $f is missing"
done
pass "UDFs present"
for d in IPProtocols TCPFlags autnums geo_asn geo_country SamplerConfig InterfaceNames \
	VlanNames HostNames EtherTypes; do
	[[ $(ch "SELECT count() FROM system.dictionaries WHERE name = '$d'") == 1 ]] ||
		fail "dictionary $d is missing"
done
# Dictionaries load lazily; force each one so a broken source shows up here.
for d in $(ch "SELECT name FROM system.dictionaries"); do
	ch "SYSTEM RELOAD DICTIONARY $d" >/dev/null || fail "dictionary $d failed to load"
done
pass "dictionaries present and loadable"
ch "SELECT dictGet('IPProtocols', 'Name', toUInt64(6)), dictGet('TCPFlags', 'Name', toUInt64(2)),
	dictGet('EtherTypes', 'Name', toUInt64(2048)) FORMAT TSV" | grep -q . ||
	fail "static dictionaries are empty"
pass "static dictionaries: $(ch "SELECT dictGet('IPProtocols', 'Name', toUInt64(6)) || ' / ' || dictGet('EtherTypes', 'Name', toUInt64(2048))")"

# --- 7. enrichment path (hand-written geo rows) --------------------------------
log "enrichment through geo.asn / geo.country"
geo_rows=$(ch "SELECT (SELECT count() FROM geo.asn) + (SELECT count() FROM geo.country)")
if ((geo_rows == 0)); then
	# IPv4-mapped key form and a plain IPv6 catch-all.
	ch "INSERT INTO geo.asn VALUES ('::ffff:0.0.0.0/96', 64500), ('::/0', 64501)"
	ch "INSERT INTO geo.country VALUES ('::ffff:0.0.0.0/96', 'NL'), ('::/0', 'DE')"
	ch "SYSTEM RELOAD DICTIONARY geo_asn"
	ch "SYSTEM RELOAD DICTIONARY geo_country"
	[[ $(ch "SELECT dictGet('geo_asn', 'asn', tuple(toIPv6('192.0.2.1')))") == 64500 ]] ||
		fail "geo_asn lookup of an IPv4-mapped address failed"
	[[ $(ch "SELECT dictGet('geo_country', 'country', tuple(toIPv6('2001:db8::1')))") == DE ]] ||
		fail "geo_country lookup of an IPv6 address failed"
	pass "dictGet on IPv4-mapped and IPv6 keys"
	# The materialised view's dictionary references are cached; give it a fresh batch.
	T1=$(ch "SELECT toUnixTimestamp(now())")
	send_flows 10
	n=$(wait_rows "TimeReceived >= $T1 AND SrcCountry = 'NL'")
	((n > 0)) || fail "flows were not enriched with the geo.country data"
	pass "generator flows enriched: $n rows with SrcCountry='NL'"
	# The exporter sent no AS numbers: both come from geo_asn (IPv4-mapped and IPv6 keys).
	SEQ=$((RANDOM * 32768 + RANDOM))
	synthetic_flow "$SEQ" 0
	got=$(flow_fields "$SEQ") || fail "synthetic flow did not arrive"
	want="::ffff:192.0.2.1 ::ffff:198.51.100.1 2001:db8::1 34525 1 64500 64501 NL DE"
	[[ "$got" == "$want" ]] || fail "synthetic flow enriched as '$got', want '$want'"
	pass "synthetic flow enriched: SrcAS/DstAS 64500/64501, countries NL/DE"
	# An AS number sent by the exporter wins over the lookup.
	SEQ=$((RANDOM * 32768 + RANDOM))
	synthetic_flow "$SEQ" 65000
	got=$(flow_fields "$SEQ") || fail "synthetic flow did not arrive"
	[[ $(echo "$got" | cut -d' ' -f6) == 65000 ]] || fail "exporter SrcAS was overwritten: $got"
	pass "exporter AS number kept"
	ch "TRUNCATE TABLE geo.asn"
	ch "TRUNCATE TABLE geo.country"
	ch "SYSTEM RELOAD DICTIONARY geo_asn"
	ch "SYSTEM RELOAD DICTIONARY geo_country"
else
	echo "skipped: geo tables already hold data (geoip loader configured)"
fi

# --- 8. ClickHouse restart ----------------------------------------------------
log "docker compose restart clickhouse"
"${COMPOSE[@]}" restart clickhouse >/dev/null
"${COMPOSE[@]}" up -d --wait --wait-timeout 300 clickhouse >/dev/null
T2=$(ch "SELECT toUnixTimestamp(now())")
send_flows 10
n=$(wait_rows "TimeReceived >= $T2")
((n > 0)) || fail "ingestion did not resume after restarting clickhouse"
pass "ingestion resumed after restart: $n rows"

log "PASS"
