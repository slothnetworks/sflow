#!/usr/bin/env bash
# Copy flows_raw from an old NetMeta (k3s) ClickHouse into the new compose stack.
#
# The old server is only ever sent SELECTs, as HTTP GET requests (which ClickHouse
# itself forces to read-only). All writes happen on the NEW ClickHouse.
#
# Usage:
#   OLD_URL=http://old-host:15003 OLD_PASSWORD=... scripts/backfill.sh [options]
#
# Options:
#   --delta-since TS          Copy only rows with TimeReceived > TS (unix seconds), for
#                             the catch-up after cutover. Without it, copy partitions.
#   --force-partition DATE    Drop DATE (YYYY-MM-DD) on the new server and copy it again.
#                             May be given more than once.
#   --force                   In delta mode, run even if the window overlaps a delta
#                             already recorded in the state file.
#   --enrich-country          Fill SrcCountry/DstCountry from the geo_country dictionary
#                             while copying (default: leave them '').
#   --dry-run                 Print the plan, copy nothing.
#   -h, --help                Show this help.
#
# Environment:
#   OLD_URL          old ClickHouse HTTP endpoint, e.g. http://old-host:15003 (required)
#   OLD_USER         old ClickHouse user (default: admin)
#   OLD_PASSWORD     old ClickHouse password (required)
#   OLD_TABLE        source table (default: default.flows_raw)
#   OLD_SETTINGS     extra settings for the copy query on the old server, as URL parameters,
#                    e.g. max_threads=2 to lower its load (default: none)
#   ENV_FILE         new stack's env file to read CLICKHOUSE_USER/CLICKHOUSE_PASSWORD from
#                    (default: .env next to compose.yaml)
#   CLICKHOUSE_USER, CLICKHOUSE_PASSWORD
#                    new ClickHouse credentials (override ENV_FILE)
#   NEW_CLIENT       command that runs clickhouse-client on the new server, reading SQL
#                    from -q (default: docker compose exec -T clickhouse clickhouse-client)
#   CUTOFF_LAG       partition mode copies rows up to now - CUTOFF_LAG seconds, so rows
#                    still in the old Kafka buffer are left for the delta (default: 300)
#   GEO_COUNTRY_ATTR attribute of geo_country used by --enrich-country (default: country)
#   BACKFILL_STATE   file that records completed delta windows (default: .backfill-state)
#
# Note: the old server must be reachable from inside the NEW ClickHouse container at
# OLD_URL (url() runs there), and from this shell (for counting).

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

usage() { sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
log()   { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die()   { log "ERROR: $*"; exit 1; }

delta_since=""
force=0
enrich=0
dry_run=0
declare -A force_part=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --delta-since)     delta_since=${2:?--delta-since needs a unix timestamp}; shift 2 ;;
        --force-partition) [[ ${2:-} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "--force-partition needs YYYY-MM-DD"
                           force_part[$2]=1; shift 2 ;;
        --force)           force=1; shift ;;
        --enrich-country)  enrich=1; shift ;;
        --dry-run)         dry_run=1; shift ;;
        -h|--help)         usage 0 ;;
        *)                 log "unknown argument: $1"; usage 2 ;;
    esac
done
[[ -z $delta_since || $delta_since =~ ^[0-9]+$ ]] || die "--delta-since must be unix seconds"

: "${OLD_URL:?set OLD_URL, e.g. http://old-host:15003}"
: "${OLD_PASSWORD:?set OLD_PASSWORD}"
OLD_USER=${OLD_USER:-admin}
OLD_TABLE=${OLD_TABLE:-default.flows_raw}
OLD_SETTINGS=${OLD_SETTINGS:-}
OLD_URL=${OLD_URL%/}
CUTOFF_LAG=${CUTOFF_LAG:-300}
GEO_COUNTRY_ATTR=${GEO_COUNTRY_ATTR:-country}
BACKFILL_STATE=${BACKFILL_STATE:-$here/.backfill-state}

# New-stack credentials: environment first, then the compose env file.
env_get() {  # env_get FILE KEY -> value of the last KEY=... line (no shell evaluation)
    [[ -f $1 ]] || return 0
    sed -n "s/^[[:space:]]*$2=//p" "$1" | tail -n1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}
ENV_FILE=${ENV_FILE:-$here/.env}
CLICKHOUSE_USER=${CLICKHOUSE_USER:-$(env_get "$ENV_FILE" CLICKHOUSE_USER)}
CLICKHOUSE_USER=${CLICKHOUSE_USER:-admin}
CLICKHOUSE_PASSWORD=${CLICKHOUSE_PASSWORD:-$(env_get "$ENV_FILE" CLICKHOUSE_PASSWORD)}

read -r -a new_client <<<"${NEW_CLIENT:-docker compose --project-directory $here exec -T clickhouse clickhouse-client}"
new_client+=(--user "$CLICKHOUSE_USER")
[[ -n $CLICKHOUSE_PASSWORD ]] && new_client+=(--password "$CLICKHOUSE_PASSWORD")

# The 42 columns of the original flows_raw (sql/migrations/0001_create_flows_raw.sql).
# The new table has extra columns (SrcCountry, DstCountry) that take their defaults.
cols="Date, FlowType, SequenceNum, TimeReceived, SamplingRate, FlowDirection, SamplerAddress,
TimeFlowStart, TimeFlowEnd, Bytes, Packets, SrcAddr, DstAddr, EType, Proto, SrcPort, DstPort,
InIf, OutIf, SrcMac, DstMac, SrcVlan, DstVlan, VlanId, IngressVrfId, EgressVrfId, IPTos,
ForwardingStatus, IPTTL, TCPFlags, IcmpType, IcmpCode, IPv6FlowLabel, FragmentId,
FragmentOffset, BiFlowDirection, SrcAS, DstAS, NextHop, NextHopAS, SrcNet, DstNet"
cols=$(tr -s ' \n' ' ' <<<"$cols")

# ---------------------------------------------------------------------------------------

sqlq() {  # quote a string as a ClickHouse SQL literal
    local s=${1//\\/\\\\}
    s=${s//\'/\\\'}
    printf "'%s'" "$s"
}

urlencode() {
    local LC_ALL=C s=$1 out="" c i
    for ((i = 0; i < ${#s}; i++)); do
        c=${s:i:1}
        case "$c" in
            [a-zA-Z0-9.~_-]) out+=$c ;;
            *) out+=$(printf '%%%02X' "'$c") ;;
        esac
    done
    printf '%s' "$out"
}

# Read-only query against the old server: HTTP GET, which ClickHouse runs read-only.
old_query() {
    curl -sS --fail-with-body -G "$OLD_URL/" \
        -H @<(printf 'X-ClickHouse-User: %s\nX-ClickHouse-Key: %s\n' "$OLD_USER" "$OLD_PASSWORD") \
        --data-urlencode "query=$1"
}

# Query against the new server. log_queries=0 keeps the old password (in url() headers)
# out of the new server's system.query_log.
new_query() {
    "${new_client[@]}" --log_queries=0 -q "$1"
}

# INSERT INTO flows_raw SELECT <rows matching WHERE> FROM the old server.
copy_rows() {
    local where=$1 remote url ins_cols sel_cols
    remote="SELECT $cols FROM $OLD_TABLE WHERE $where FORMAT Native"
    url="$OLD_URL/?enable_http_compression=1${OLD_SETTINGS:+&$OLD_SETTINGS}&query=$(urlencode "$remote")"
    ins_cols=$cols
    sel_cols=$cols
    if ((enrich)); then
        ins_cols+=", SrcCountry, DstCountry"
        sel_cols+=", dictGetOrDefault('geo_country', $(sqlq "$GEO_COUNTRY_ATTR"), SrcAddr, '')"
        sel_cols+=", dictGetOrDefault('geo_country', $(sqlq "$GEO_COUNTRY_ATTR"), DstAddr, '')"
    fi
    # url(URL, format, structure, compression, headers): Native carries its own schema
    # ('auto'), but the zstd body must be named explicitly; url() does not decode
    # Content-Encoding by itself.
    new_query "INSERT INTO flows_raw ($ins_cols)
SELECT $sel_cols
FROM url($(sqlq "$url"), 'Native', 'auto', 'zstd',
         headers('X-ClickHouse-User' = $(sqlq "$OLD_USER"),
                 'X-ClickHouse-Key' = $(sqlq "$OLD_PASSWORD"),
                 'Accept-Encoding' = 'zstd'))
SETTINGS http_receive_timeout = 600, max_execution_time = 0"
}

# Sanity checks: both sides reachable, schemas look right.
old_version=$(old_query "SELECT version()") || die "cannot query OLD_URL=$OLD_URL"
new_version=$(new_query "SELECT version()") || die "cannot run NEW_CLIENT"
log "old ClickHouse $old_version at $OLD_URL, new ClickHouse $new_version"
[[ $(new_query "EXISTS TABLE flows_raw") == 1 ]] || die "flows_raw missing on the new server (did migrate run?)"

# ---------------------------------------------------------------------------------------
# Delta mode
# ---------------------------------------------------------------------------------------
if [[ -n $delta_since ]]; then
    read -r old_n old_max < <(old_query "SELECT count(), max(TimeReceived) FROM $OLD_TABLE WHERE TimeReceived > $delta_since FORMAT TSV")
    if ((old_n == 0)); then
        log "old server has no rows with TimeReceived > $delta_since; nothing to do"
        exit 0
    fi
    window="TimeReceived > $delta_since AND TimeReceived <= $old_max"
    log "delta window ($delta_since, $old_max]: $old_n rows on the old server"

    if [[ -f $BACKFILL_STATE ]] && ((!force)); then
        while read -r kind from to _; do
            [[ $kind == delta ]] || continue
            if ((delta_since < to && old_max > from)); then
                die "window overlaps delta ($from, $to] already copied (see $BACKFILL_STATE); use --delta-since $to, or --force"
            fi
        done <"$BACKFILL_STATE"
    fi

    new_before=$(new_query "SELECT count() FROM flows_raw WHERE $window")
    log "new server already has $new_before rows in the window (live ingestion after cutover)"
    if ((dry_run)); then log "dry run: would copy $old_n rows"; exit 0; fi

    copy_rows "$window"
    new_after=$(new_query "SELECT count() FROM flows_raw WHERE $window")
    inserted=$((new_after - new_before))
    if ((inserted < old_n)); then
        die "delta copied $inserted rows, expected $old_n"
    fi
    ((inserted == old_n)) || log "note: $((inserted - old_n)) extra rows arrived from live ingestion meanwhile"
    printf 'delta %s %s %s %s\n' "$delta_since" "$old_max" "$old_n" "$(date -u +%FT%TZ)" >>"$BACKFILL_STATE"
    log "delta OK: $old_n rows; next catch-up (if the old server is still receiving): --delta-since $old_max"
    exit 0
fi

# ---------------------------------------------------------------------------------------
# Partition mode
# ---------------------------------------------------------------------------------------
cutoff=$(( $(date -u +%s) - CUTOFF_LAG ))
log "copying rows with TimeReceived <= $cutoff ($(date -u -d "@$cutoff" +%FT%TZ))"

mapfile -t parts < <(old_query "SELECT partition, sum(rows) FROM system.parts WHERE active AND database = 'default' AND table = 'flows_raw' GROUP BY partition ORDER BY partition FORMAT TSV")
((${#parts[@]})) || die "no active flows_raw partitions on the old server"

failed=0
for line in "${parts[@]}"; do
    read -r d part_rows <<<"$line"
    [[ $d =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "unexpected partition id '$d' (expected PARTITION BY Date)"
    dq=$(sqlq "$d")
    old_n=$(old_query "SELECT count() FROM $OLD_TABLE WHERE Date = $dq AND TimeReceived <= $cutoff")
    new_n=$(new_query "SELECT count() FROM flows_raw WHERE Date = $dq")

    if [[ -n ${force_part[$d]:-} ]] && ((new_n > 0)); then
        if ((dry_run)); then
            log "$d: would drop $new_n rows on the new server (--force-partition)"
        else
            log "$d: dropping $new_n rows on the new server (--force-partition)"
            new_query "ALTER TABLE flows_raw DROP PARTITION $dq"
        fi
        new_n=0
    fi

    if ((old_n == 0)); then
        log "$d: no rows up to the cutoff, skipping"
        continue
    fi
    if ((new_n == old_n)); then
        log "$d: complete ($new_n rows), skipping"
        continue
    fi
    if ((new_n > 0)); then
        # Copied by an earlier run with an earlier cutoff? Then it matches the old rows up
        # to its own high-water mark, and the remainder is for --delta-since.
        new_max=$(new_query "SELECT max(TimeReceived) FROM flows_raw WHERE Date = $dq")
        old_upto=$(old_query "SELECT count() FROM $OLD_TABLE WHERE Date = $dq AND TimeReceived <= $new_max")
        if ((new_n == old_upto)); then
            log "$d: complete up to TimeReceived $new_max ($new_n rows), skipping; the rest comes with --delta-since"
            continue
        fi
        log "$d: new server has $new_n rows, old has $old_n (up to the cutoff); refusing to insert twice."
        log "$d: rerun with --force-partition $d to drop and copy it again"
        failed=1
        continue
    fi

    if ((dry_run)); then
        log "$d: would copy $old_n rows (partition holds $part_rows)"
        continue
    fi
    log "$d: copying $old_n rows"
    start=$SECONDS
    copy_rows "Date = $dq AND TimeReceived <= $cutoff"
    new_n=$(new_query "SELECT count() FROM flows_raw WHERE Date = $dq")
    if ((new_n != old_n)); then
        log "$d: MISMATCH after copy: new $new_n, old $old_n; rerun with --force-partition $d"
        failed=1
        continue
    fi
    log "$d: OK, $new_n rows in $((SECONDS - start)) s"
done

((dry_run)) && exit 0
hw=$(new_query "SELECT max(TimeReceived) FROM flows_raw")
if ((failed)); then
    die "some partitions failed; see above"
fi
log "all partitions match. High-water mark on the new server: $hw"
log "after cutover, run: scripts/backfill.sh --delta-since $hw"
