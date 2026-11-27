# Operations

Day-2 tasks for the Docker Compose stack. Run the commands from the repository directory (where `compose.yaml`
and `.env` are).

## Service status and logs

```bash
docker compose ps                 # all services should be "healthy"; migrate "exited (0)"
docker compose logs -f goflow2    # or kafka, clickhouse, migrate, geoip, grafana
```

## SQL console

```bash
docker compose exec clickhouse clickhouse-client
```

The container's `CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD` environment logs the client in as `admin`. If it asks
for a password, pass it explicitly: `clickhouse-client --user admin --password "$CLICKHOUSE_PASSWORD"`. Add `-mn`
for multi-line input.

Remotely, ClickHouse HTTP is on port 15003:

```bash
curl -u admin:"$CLICKHOUSE_PASSWORD" 'http://<host>:15003/' --data-binary 'SELECT count() FROM flows_raw'
```

Quick health check of ingestion:

```sql
SELECT count(), max(TimeReceived), now() - toDateTime(max(TimeReceived)) AS lag_s
FROM flows_raw WHERE Date = today();
```

## Kafka

Consumer group lag (ClickHouse consumes as group `clickhouse`). A small, stable LAG is normal; a growing one means
ClickHouse isn't keeping up or is down:

```bash
docker compose exec kafka /opt/kafka/bin/kafka-consumer-groups.sh \
    --bootstrap-server localhost:9092 --describe --all-groups
```

Look at raw messages on the topic (protobuf, so mostly binary):

```bash
docker compose exec kafka /opt/kafka/bin/kafka-console-consumer.sh \
    --bootstrap-server localhost:9092 --topic flow-messages --max-messages 5
```

Kafka buffers flows while ClickHouse is restarting or down, bounded by `KAFKA_RETENTION_BYTES` and 7 days.
`docker compose restart clickhouse` loses nothing as long as it comes back within that window.

## goflow2 metrics

goflow2 runs on the host network, and its Prometheus endpoint listens on loopback only:

```bash
curl -s http://127.0.0.1:18080/metrics | grep -v '^#' | grep -E 'goflow2_flow_(traffic|dropped|decoder|process)'
```

Useful counters: `goflow2_flow_traffic_packets_total` (received datagrams, by exporter and port),
`goflow2_flow_dropped_packets_total` (dropped because goflow2 could not keep up),
`goflow2_flow_decoder_error_total` (undecodable packets) and `goflow2_flow_process_nf_errors_total` (NetFlow
template problems).

To see whether an exporter's packets arrive at all:

```bash
sudo tcpdump -ni any 'udp and (port 2055 or port 2056 or port 6343)'
```

## Retention

Flow retention is `RETENTION_DAYS` in `.env` (default 7). The `migrate` service applies it as the `flows_raw` TTL
whenever it runs:

```bash
$EDITOR .env               # RETENTION_DAYS=14
docker compose up -d migrate
docker compose logs migrate
```

Check the TTL in effect:

```sql
SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'flows_raw';
```

Lowering the TTL deletes the expired partitions at the next merge. Raising it only keeps data that hasn't been
deleted yet.

## Partitions

`flows_raw` is partitioned by day (`Date`). List them, oldest first:

```sql
SELECT partition, sum(rows) AS rows, formatReadableSize(sum(bytes_on_disk)) AS size
FROM system.parts
WHERE active AND database = 'default' AND table = 'flows_raw'
GROUP BY partition ORDER BY partition;
```

Drop a day immediately, for example to free disk space before the TTL would:

```sql
ALTER TABLE flows_raw DROP PARTITION '2026-11-20';
```

Disk usage per table:

```sql
SELECT database, table, formatReadableSize(sum(bytes_on_disk)) AS size
FROM system.parts WHERE active GROUP BY database, table ORDER BY sum(bytes_on_disk) DESC LIMIT 15;
```

## System logs stay bounded

On the old k3s deployment, ClickHouse's `trace_log`, `metric_log` and `asynchronous_metric_log` had no TTL and
filled the disk. `config/clickhouse/config.d/netmeta.xml` now gives every system log a TTL (3 to 14 days) and
disables the logs NetMeta doesn't need, including `trace_log`. To check, run the disk usage query above and look
at the `system` rows. They should stay roughly constant from week to week. The TTLs in effect:

```sql
SELECT name, extract(engine_full, 'TTL [^S]+') AS ttl
FROM system.tables WHERE database = 'system' AND name LIKE '%_log';
```

A system log table created by an older config keeps its old definition: ClickHouse renames it to
`<name>_0` and creates a new one. Drop the `_0` tables once you no longer need them.

## User data (samplers, interfaces, VLANs, hosts, own ASNs)

Edit the TSV files in `config/data/`, described in [config/data/README.md](../config/data/README.md). ClickHouse
re-reads them within 60 seconds; no restart is needed. To reload immediately, or to check for errors:

```sql
SYSTEM RELOAD DICTIONARIES;
SELECT name, status, element_count, last_exception FROM system.dictionaries;
```

## Editing isIncomingFlow

`isIncomingFlow` decides which flows the dashboards count as "in" and which as "out". sFlow carries no direction,
so with the default every sFlow flow counts as "in". Edit `config/clickhouse/isIncomingFlow.sql` (it has
examples), then re-run migrate, which re-creates the function:

```bash
docker compose up -d --force-recreate migrate
docker compose logs migrate
```

Test it before relying on the dashboards:

```sql
SELECT isIncomingFlow(SamplerAddress, SrcAddr, DstAddr, SrcAS, DstAS, FlowDirection) AS incoming, count()
FROM flows_raw WHERE Date = today() GROUP BY incoming;
```

Keep the argument list unchanged, because the dashboard queries call the function with exactly these arguments.

## ASN and country lookups

Enrichment is optional; see [enrichment.md](enrichment.md). The lookups the dashboards and the ingest view use:

```sql
-- AS number and country for an address (0 / '' when unknown or no provider is configured)
SELECT dictGet('geo_asn', 'asn', toIPv6('::ffff:1.1.1.1'));
SELECT dictGet('geo_country', 'country', toIPv6('::ffff:1.1.1.1'));

-- AS name (public names from geo.asn_names merged with config/data/own_asns.tsv)
SELECT dictGetString('autnums', 'name', toUInt64(13335));
SELECT ASNToString(13335);

-- Loaded data
SELECT count() FROM geo.asn;
SELECT count() FROM geo.country;
SELECT count() FROM geo.asn_names;

-- Top source ASes today
SELECT SrcAS, ASNToString(SrcAS) AS name, count() FROM flows_raw
WHERE Date = today() GROUP BY SrcAS ORDER BY count() DESC LIMIT 20;
```

## Upgrading

```bash
git pull
docker compose pull
docker compose up -d --build
```

`migrate` applies any new SQL migrations on start-up. Check `docker compose logs migrate` afterwards.
