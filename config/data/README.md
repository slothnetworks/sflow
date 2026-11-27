# NetMeta user data

These tab-separated files are mounted into ClickHouse and reloaded every 60 seconds.
No restart is needed after editing them. Leave a file empty if you don't need it.

Sampler and device addresses are IPv6 strings. IPv4 samplers use the mapped form, `::ffff:192.0.2.1`.

| File | Columns (tab-separated) | Used by |
|------|-------------------------|---------|
| `samplers.tsv` | `Device`, `SamplingRate` (or `NULL` for auto), `Description` (or `NULL`), `AnonymizeAddresses` (`true`/`false`) | `SamplerToString`, sampling-rate override, address anonymisation |
| `interface_names.tsv` | `Device`, `Index`, `Description` | `InterfaceToString` |
| `vlan_names.tsv` | `Device`, `Index` (VLAN id), `Description` | `VLANToString` |
| `host_names.tsv` | `Sampler`, `Device` (host address), `Description` | `HostToString` |
| `own_asns.tsv` | `asnum`, `name`, `country` | `ASNToString`, merged with the public AS names from risinfo |

Example `samplers.tsv` line:

```
::ffff:192.0.2.1	NULL	core-router-1	false
```
