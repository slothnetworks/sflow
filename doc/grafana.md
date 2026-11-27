# Grafana

## Bundled Grafana

The `grafana` service runs Grafana 13 on `http://<host>:15001` (`GRAFANA_HTTP_PORT`), or on
`https://<PUBLIC_HOSTNAME>:15002` behind Caddy with the `tls` profile. Log in as `admin` with
`GRAFANA_ADMIN_PASSWORD` from `.env`.

Everything is provisioned from the repository, so a fresh `docker compose up -d` comes up ready:

- **Plugins:** `grafana-clickhouse-datasource` and `netsage-sankey-panel`, installed at start-up through
  `GF_PLUGINS_PREINSTALL`. The container therefore needs to reach grafana.com the first time it starts.
- **Datasource:** `config/grafana/provisioning/datasources/clickhouse.yaml`.
  - The uid is fixed at `b315efc1-b699-4ba8-a3a2-d6c0e711c5fd`. Dashboards exported from older NetMeta
    deployments (such as "Sflow Overview") reference this uid, so they work unmodified.
  - It connects as the read-only ClickHouse user `grafana` (password `CLICKHOUSE_GRAFANA_PASSWORD`), defined in
    `config/clickhouse/users.d/netmeta.xml`. `readonly=2` lets the plugin set per-query settings but forbids
    writes.
- **Dashboards:** every JSON file in `config/grafana/dashboards/` is loaded into Grafana and re-read every 60 s.
  Provisioned dashboards can't be saved from the UI. To change one, edit it in the UI, export it as JSON (classic
  format), and replace the file. To keep a private copy instead, use "Save as".

New dashboards that you create in the UI are stored in the `grafana-data` volume.

## Google OAuth

Grafana can sign users in with Google. Create an OAuth client ("Web application") in the Google Cloud console,
with the redirect URI `<root URL>/login/google`, for example `https://flowmon.example.com:15002/login/google`.
Then set in `.env`:

| Variable                         | Meaning                                                           |
|----------------------------------|-------------------------------------------------------------------|
| `GRAFANA_GOOGLE_CLIENT_ID`       | OAuth client id. Google sign-in is enabled when this is set.      |
| `GRAFANA_GOOGLE_CLIENT_SECRET`   | OAuth client secret                                               |
| `GRAFANA_GOOGLE_ALLOWED_DOMAINS` | Google Workspace domains allowed to sign in (space-separated)    |
| `GRAFANA_DEFAULT_ROLE`           | Role for new users: `Viewer` (default), `Editor` or `Admin`      |

The root URL comes from `PUBLIC_HOSTNAME` and the port, so set `PUBLIC_HOSTNAME` to the name users browse to.
Apply the change with `docker compose up -d grafana`.

## Using an existing Grafana

You don't have to use the bundled Grafana. Any Grafana with the
[ClickHouse datasource plugin](https://grafana.com/grafana/plugins/grafana-clickhouse-datasource/) (4.x) can query
NetMeta directly:

1. Add a datasource of type ClickHouse:
   - Server address: the NetMeta host, port `15003` (`CLICKHOUSE_HTTP_PORT`), protocol **HTTP**, TLS off.
   - Username and password: `grafana` / `CLICKHOUSE_GRAFANA_PASSWORD` for read-only access. (`admin` also works
     but can write.)
   - Default database: `default`.
2. If you import NetMeta's dashboards, give the datasource the uid `b315efc1-b699-4ba8-a3a2-d6c0e711c5fd` (set it
   in datasource provisioning; the UI can't change a uid), or pick your datasource when importing.
3. Import the JSON files from `config/grafana/dashboards/`. "Sflow Overview" also uses the
   `netsage-sankey-panel` plugin.

Port 15003 is plain HTTP, so credentials and query results cross the network unencrypted. Restrict it with a
firewall to the Grafana hosts that need it, or keep it on a private network.
