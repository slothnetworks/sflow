-- Turns an IPv4 CIDR into its IPv4-mapped IPv6 form ('10.0.0.0/8' -> '::ffff:10.0.0.0/104').
-- Used directly by dashboard queries.
CREATE OR REPLACE FUNCTION toIPv6Net AS (Net) ->
    if(isIPv4String(splitByChar('/', Net)[1]),
       concat('::ffff:', arrayStringConcat(arrayMap((v, i) -> if(i, toString(toInt8(v) + 96), v), splitByChar('/', Net), [false, true]), '/')),
       Net);
