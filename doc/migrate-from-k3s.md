# Migrating from the k3s deployment

This runbook moves an older NetMeta deployment (k3s, ClickHouse 23.x, `default.flows_raw` partitioned by `Date`)
to the Docker Compose stack on a new host, with no gap in the data.

**Nothing is changed on the old host.** The procedure only reads from it, through its existing ClickHouse HTTP
endpoint on port 15003, with `SELECT` queries sent as HTTP GET requests, which ClickHouse always runs read-only.
Everything else happens on the new host, on the exporters (routers), and in your own Grafana instances.

**The old k3s deployment is not decommissioned by this procedure.** It keeps running and stays available for
rollback. Deciding when to shut it down, and doing it, is a separate step outside this runbook.

## Overview

1. Bring up the new stack, receiving no flows yet.
2. Backfill: copy every day partition of `flows_raw` from the old server.
3. Cutover: re-point the exporters to the new host, then copy the rows the old server received in the meantime.
4. Re-point your Grafana datasources to the new host.
5. Verify, and keep the old stack for rollback.

The old deployment holds about 7 days of flows (TTL 1 week), roughly 31 GiB and 1.3 billion rows, and ingests
about 2,000 flows/s. Expect the backfill to take somewhere between tens of minutes and a few hours, depending on
the network between the hosts and the old host's disk.

## Prerequisites

- A new host with Docker Engine and the Compose plugin, and at least **2x the old `flows_raw` size** free (about
  60 GiB for the numbers above), plus room for growth.
- Network:
  - The new host, including its Docker containers, can reach `http://<old-host>:15003`.
    `url()` runs inside the new ClickHouse container, so test from there (see step 1).
  - Exporters can reach the new host on UDP 2055, 2056 and 6343.
  - Your Grafana instances can reach the new host on TCP 15003.
- The old ClickHouse `admin` password, i.e. the one your Grafana datasources use today.
- `bash` and `curl` on the new host. The script runs there, next to `compose.yaml`.
- An exporter change window for step 3, and a list of every exporter and its current collector address.

Check what the old server holds. This is read-only, and you can run it from anywhere that reaches it:

```bash
OLD=http://<old-host>:15003
curl -sG "$OLD/" -H 'X-ClickHouse-User: admin' -H "X-ClickHouse-Key: $OLD_PASSWORD" --data-urlencode \
  "query=SELECT partition, sum(rows), formatReadableSize(sum(bytes_on_disk)) FROM system.parts
         WHERE active AND database='default' AND table='flows_raw' GROUP BY partition ORDER BY partition"
```

## 1. Bring up the new stack without exporters

```bash
git clone https://github.com/slothnetworks/sflow netmeta && cd netmeta
cp .env.example .env
$EDITOR .env
docker compose up -d
docker compose ps        # everything healthy, migrate exited 0
```

In `.env`:

- Set new passwords. The new `admin` password does not need to match the old one. If it does, your Grafana
  datasources only need a new host name in step 4.
- Set `RETENTION_DAYS` to at least the old retention (7). The new `flows_raw` TTL deletes older backfilled days
  otherwise.
- Keep the default ports (2055, 2056, 6343, 15003), so that the exporters and Grafana only need a new address.
- If you want countries on the backfilled rows, configure `GEOIP_PROVIDERS` now and wait for the geoip service to
  load its data (see [enrichment.md](enrichment.md)). Then use `--enrich-country` in step 2. This is optional.

Don't run `make e2e` or a flow generator on this host: test flows would mix with production data.

Check that the new ClickHouse container can reach the old server:

```bash
docker compose exec clickhouse wget -qO- "http://<old-host>:15003/ping"    # prints "Ok."
```

## 2. Backfill

`scripts/backfill.sh` lists the old partitions and copies them one day at a time. For each day it runs, on the
**new** ClickHouse:

```sql
INSERT INTO flows_raw (<the 42 original columns>)
SELECT <the 42 original columns>
FROM url('http://<old-host>:15003/?enable_http_compression=1&query=SELECT%20...%20FORMAT%20Native',
         'Native', 'auto', 'zstd',
         headers('X-ClickHouse-User' = 'admin', 'X-ClickHouse-Key' = '…', 'Accept-Encoding' = 'zstd'))
```

The old server streams the day as zstd-compressed Native blocks. The new `flows_raw` has two extra columns,
`SrcCountry` and `DstCountry`; they get their default `''`, or the `geo_country` lookup with `--enrich-country`.

Dry run first, then copy. Run it in `tmux` or `screen`, because it takes a while:

```bash
export OLD_URL=http://<old-host>:15003 OLD_USER=admin OLD_PASSWORD='<old admin password>'
scripts/backfill.sh --dry-run
scripts/backfill.sh                    # optionally: --enrich-country, OLD_SETTINGS=max_threads=4
```

The new-stack credentials are read from `.env`.

What the script does and guarantees:

- It only copies rows with `TimeReceived <= now - 300 s`. Today's partition is still growing on the old server,
  and the rest of it comes with the delta in step 3.
- It verifies each day after copying: the new row count must equal the old count.
- It can be resumed. Re-running skips days that are complete, including a today partition that a previous run
  copied up to its own cutoff. You can stop it at any time and run it again.
- It never inserts twice. A day that has some rows on the new server but not the right number (an interrupted
  copy) is reported and left alone. To drop that day on the new server and copy it again, run
  `scripts/backfill.sh --force-partition YYYY-MM-DD`.
- The old server's TTL keeps dropping its oldest day. A day that disappears from the old server before it was
  copied is simply not listed any more.
- It runs the inserts with `log_queries=0`, so the old password in the `url()` headers is not stored in the new
  server's `system.query_log`.

At the end it prints the high-water mark, the newest `TimeReceived` on the new server:

```text
all partitions match. High-water mark on the new server: 1795764799
after cutover, run: scripts/backfill.sh --delta-since 1795764799
```

Before the change window you can shrink the final catch-up by running that delta command now. The script
records each delta window in `.backfill-state` and prints the next `--delta-since` value to use.

## 3. Cutover

1. **Re-point the exporters.** On each router or host, change the sFlow collector or NetFlow/IPFIX destination from
   the old host to the new host, keeping the same port. Note the time.
   - sFlow → UDP 6343; NetFlow/IPFIX → UDP 2055 (or 2056 if that's what the device used).
   - Each exporter sends to exactly one collector at a time, so the two servers never hold the same flow, and a
     device can be moved whenever convenient.
2. **Check that the new host receives flows:**

   ```bash
   docker compose exec clickhouse clickhouse-client -q \
     "SELECT SamplerAddress, count(), max(TimeReceived) FROM flows_raw
      WHERE TimeReceived > toUnixTimestamp(now()) - 300 GROUP BY SamplerAddress"
   ```

   Every exporter should appear. If one doesn't, check the firewall and `tcpdump -ni any udp port 6343`.
3. **Wait until the old server has stopped receiving.** Its newest `TimeReceived` should stop moving for a few
   minutes, which also means its Kafka buffer has drained:

   ```bash
   curl -sG "$OLD_URL/" -H "X-ClickHouse-User: $OLD_USER" -H "X-ClickHouse-Key: $OLD_PASSWORD" \
     --data-urlencode "query=SELECT max(TimeReceived), toUnixTimestamp(now()) FROM default.flows_raw"
   ```

4. **Copy the delta:**

   ```bash
   scripts/backfill.sh --delta-since <high-water mark from step 2, or the last delta's end>
   ```

   This copies every old row with `TimeReceived` after that value, up to the old server's current maximum,
   and checks that the new server gained at least that many rows. If a straggling exporter was still sending to
   the old host, run the delta again later with the `--delta-since` value it prints. Overlapping windows are
   refused (`--force` overrides that).

## 4. Re-point Grafana

The bundled Grafana on the new host (`:15001`) works immediately and includes the Sflow Overview dashboard.

For your existing Grafana 13 instances (the docker `grafana` container and the k8s `grafana` namespace), edit the
ClickHouse datasource with uid `b315efc1-b699-4ba8-a3a2-d6c0e711c5fd`:

- Server address: the new host. Port `15003`, protocol HTTP, TLS off: the same as today.
- User and password: `admin` with the new `CLICKHOUSE_PASSWORD`, or the read-only `grafana` user with
  `CLICKHOUSE_GRAFANA_PASSWORD`, which is the better choice for dashboards.
- If the datasource is provisioned from a file, change the file and restart that Grafana. The UI can't edit
  provisioned datasources.

Keep the uid, so that "Sflow Overview" and the other dashboards bind to the datasource without changes. The
functions and dictionaries the dashboard uses exist on the new server with the same names and signatures.

## 5. Verify

Compare the two servers over the overlap window, i.e. the days copied in step 2. Pick a window that ends before
the cutover started:

```bash
FROM=$(date -u -d '3 days ago 00:00' +%s); TO=$(date -u -d 'today 00:00' +%s)
Q="SELECT Date, count(), sum(Bytes * SamplingRate), uniqExact(SamplerAddress)
   FROM flows_raw WHERE TimeReceived >= $FROM AND TimeReceived < $TO GROUP BY Date ORDER BY Date FORMAT TSV"

curl -sG "$OLD_URL/" -H "X-ClickHouse-User: $OLD_USER" -H "X-ClickHouse-Key: $OLD_PASSWORD" \
  --data-urlencode "query=${Q/flows_raw/default.flows_raw}" > old.tsv
docker compose exec -T clickhouse clickhouse-client -q "$Q" > new.tsv
diff old.tsv new.tsv && echo identical
```

Then open "Sflow Overview" in your Grafana on both datasources (or the old and the new Grafana) for the same past
time range, e.g. yesterday, and compare the totals: total traffic, top talkers, top ASes. They should match.
Expected differences:

- **After the cutover:** `EType` is filled on new rows. On the old deployment it was always 0, a bug fixed by the
  new ingest view. Backfilled rows keep the old values.
- **Countries:** backfilled rows have empty `SrcCountry`/`DstCountry` unless you used `--enrich-country`.
- **In/out split:** sFlow has no flow direction, so with the default `isIncomingFlow` everything counts as "in",
  just as on the old deployment. Per-sampler rules fix that; see
  [operations.md](operations.md#editing-isincomingflow).

After cutover, check the time around the switch for a gap or a double count: the flows per minute should be
continuous.

## Rollback

The old stack was never modified and kept running throughout, so rolling back means pointing things back at it:

1. Re-point the exporters to the old host (same ports).
2. Re-point the Grafana datasources to the old host's :15003 with the old `admin` password.

The old server has no data for the time the exporters were sending to the new host. Copying it back is outside
this procedure, which never writes to the old host. Once that time is past the old 7-day TTL, the gap is gone.
The new stack can stay up for a later attempt: re-run `scripts/backfill.sh` (completed days are skipped) and then
`--delta-since`.

## Afterwards

- Keep the old deployment running until you are satisfied with the new one, and at least long enough to repeat
  the verification. **This runbook does not decommission it.** Shutting down k3s and reclaiming the old host is a
  separate decision, and you have to do it yourself.
- `.backfill-state` in the repository directory records the delta windows. Delete it after the migration.
- Your shell history holds the old admin password, through `OLD_PASSWORD`. Clear it if that matters.
