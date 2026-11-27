# GeoIP databases

Drop `.mmdb` files (MaxMind GeoLite2/GeoIP2 ASN or Country, IPinfo Lite, or any compatible
database) here and add `files` to `GEOIP_PROVIDERS` in `.env`. The geoip service reads this
directory read-only at `/etc/netmeta/geoip` and loads the data into ClickHouse.
