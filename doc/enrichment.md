# ASN and country enrichment

NetMeta can add AS numbers, AS names and countries to flows. This is optional and **off by default**.

## Nothing external by default

With `GEOIP_PROVIDERS` empty (the default in `.env.example`), no NetMeta component contacts any external service:

- the `geoip` service starts, logs "no providers configured" and stays idle: no downloads, no ClickHouse writes;
- the `geo.*` tables stay empty, so every `geo_asn`/`geo_country` lookup returns its default;
- flows still ingest normally. `SrcAS`/`DstAS` are whatever the exporter sends (0 if it sends none), and
  `SrcCountry`/`DstCountry` are `''`.

AS names in the dashboards then come only from `config/data/own_asns.tsv`.

The one outbound connection the stack makes regardless is Grafana installing its plugins from grafana.com on first
start (see [grafana.md](grafana.md)).

## How it works

```text
providers --> geoip --> geo.asn, geo.country, geo.asn_names --> dictionaries geo_asn, geo_country, autnums
```

- `geoip` downloads (or reads) the configured databases, merges them and replaces the contents of three
  ClickHouse tables: `geo.asn` (prefix → AS number), `geo.country` (prefix → ISO country code) and
  `geo.asn_names` (AS number → name, country).
- The dictionaries `geo_asn` and `geo_country` do longest-prefix matches on those tables. `autnums` merges
  `geo.asn_names` with `config/data/own_asns.tsv`; it is what `ASNToString` and the dashboards use for AS names.
- The ingest view (`sql/migrations/0006_flows_raw_view.sql`) looks the addresses up as flows arrive:
  - `SrcAS`/`DstAS`: the exporter's value if it is non-zero, otherwise `geo_asn`;
  - `SrcCountry`/`DstCountry`: `geo_country`.
- Lookups use `dictGetOrDefault`, so a missing or empty dictionary never stops ingestion.

Enrichment happens **at ingest time**. Rows already stored keep the values they had when they arrived. Enabling a
provider affects new flows only, and so does changing one.

## Providers

Set `GEOIP_PROVIDERS` in `.env` to a comma-separated list, in priority order, then run `docker compose up -d geoip`.

| Provider     | Gives                          | Needs                                    | Downloads from                    |
|--------------|--------------------------------|------------------------------------------|-----------------------------------|
| `maxmind`    | ASN, AS names, country         | `MAXMIND_ACCOUNT_ID`, `MAXMIND_LICENSE_KEY` | download.maxmind.com            |
| `ipinfo`     | ASN, AS names, country         | `IPINFO_TOKEN`                           | ipinfo.io (IPinfo Lite)           |
| `files`      | whatever the `.mmdb` files hold | `.mmdb` files in `config/geoip/`         | nothing                           |
| `routeviews` | ASN, AS names (no country)     | nothing (keyless)                        | archive.routeviews.org, ftp.ripe.net |

### maxmind

1. Create a free GeoLite2 account at <https://www.maxmind.com/en/geolite2/signup>.
2. In the account portal, under "Manage license keys", generate a license key. Note the account id shown there.
3. Set in `.env`:

   ```bash
   GEOIP_PROVIDERS=maxmind
   MAXMIND_ACCOUNT_ID=123456
   MAXMIND_LICENSE_KEY=...
   # MAXMIND_EDITIONS=GeoLite2-ASN,GeoLite2-Country   (default; GeoIP2 editions work with a paid account)
   ```

GeoLite2 data is subject to MaxMind's EULA, which requires you to keep it up to date. The default 24 h refresh
does that.

### ipinfo

1. Sign up for a free token at <https://ipinfo.io/signup>. IPinfo Lite includes ASN and country.
2. Set `GEOIP_PROVIDERS=ipinfo` and `IPINFO_TOKEN=...`.

### files

Put `.mmdb` files in `config/geoip/` and set `GEOIP_PROVIDERS=files`. MaxMind GeoLite2/GeoIP2 ASN or Country
databases, IPinfo Lite, and compatible databases are recognised. The service reads the directory read-only and
picks up changed files within an hour. This is the provider for hosts without internet access; see below.

### routeviews

Keyless. It downloads the latest public RouteViews RIB dump (about 100 MB, prefix → origin AS) and the RIPE AS
names list. It gives no country data, so pair it with another provider if you want countries.

## Priority and merging

Providers are merged in the order listed. For each prefix (and for each AS name), the first provider that has it
wins, and later providers only fill in what earlier ones don't cover. For example,
`GEOIP_PROVIDERS=maxmind,routeviews` takes MaxMind's data and adds RouteViews prefixes that MaxMind lacks.

The dictionaries match the longest prefix across the merged set, so a more specific prefix from a later provider
still applies inside a broader prefix from an earlier one.

AS names from `config/data/own_asns.tsv` are merged into `autnums` alongside `geo.asn_names`. Use it to name your
own and your peers' ASes.

## Refresh

`GEOIP_REFRESH` (default `24h`, a Go duration such as `12h` or `168h`) is how old a provider's cached download may
get before it is fetched again. The service checks at least hourly and rewrites the `geo.*` tables only when a
file changed.

Downloads are cached in the `geoip-data` volume, so restarts don't re-download. If a download fails, the service
keeps the previous data, records the error in `/status`, and retries after 30 minutes.

## Offline use

On a host with no internet access:

1. Download the `.mmdb` files on another machine. MaxMind and IPinfo both offer direct downloads from their
   account pages.
2. Copy them to `config/geoip/` on the NetMeta host.
3. Set `GEOIP_PROVIDERS=files` and run `docker compose up -d geoip`.

Replace the files periodically to keep the data current.

## Status

The `geoip` service serves `GET /status` (JSON) and `GET /healthz` on port 8080 inside the compose network. Its
image has no shell tools, so query it from the ClickHouse container:

```bash
docker compose exec clickhouse wget -qO- http://geoip:8080/status
```

The status shows, for each provider, the last successful fetch and load, the last error and the row counts, plus
the time and row counts of the last write to ClickHouse. Also check `docker compose logs geoip`.

From SQL:

```sql
SELECT count() FROM geo.asn;
SELECT count() FROM geo.country;
SELECT dictGet('geo_asn', 'asn', toIPv6('::ffff:1.1.1.1')), dictGet('geo_country', 'country', toIPv6('::ffff:1.1.1.1'));
SELECT name, status, element_count, last_exception FROM system.dictionaries WHERE name LIKE 'geo_%' OR name = 'autnums';
```

## Turning it off

Set `GEOIP_PROVIDERS=` and run `docker compose up -d geoip`. The service stops fetching, but the data already in
`geo.*` stays in use until you clear it:

```sql
TRUNCATE TABLE geo.asn; TRUNCATE TABLE geo.country; TRUNCATE TABLE geo.asn_names;
SYSTEM RELOAD DICTIONARIES;
```
