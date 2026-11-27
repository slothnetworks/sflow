-- goflow2 v2 sends addresses as raw bytes: 4 bytes for IPv4, 16 for IPv6, empty when absent.
-- IPv4 is mapped to ::ffff:a.b.c.d. (The old version guessed IPv4 from "the low 12 bytes are zero",
-- which misread IPv6 addresses like 2001:db8::.)
CREATE OR REPLACE FUNCTION ParseGoFlowAddress AS (Address) ->
    multiIf(
        length(Address) = 4,  CAST(toFixedString(concat(repeat(unhex('00'), 10), unhex('FFFF'), Address), 16), 'IPv6'),
        length(Address) = 16, CAST(toFixedString(Address, 16), 'IPv6'),
        toIPv6('::')
    );
