-- True if an IPv6 column value (IPv4 stored as ::ffff:a.b.c.d) lies in a CIDR written as IPv4 or
-- IPv6 ('192.0.2.0/24', '2001:db8::/32'). Not in the old deployment: the "Sflow Overview"
-- dashboard calls it for its host-network filter, which failed there with "function does not exist".
CREATE OR REPLACE FUNCTION IPv6CIDRMatch AS (Address, Net) ->
    isIPAddressInRange(toString(Address), toIPv6Net(Net));
