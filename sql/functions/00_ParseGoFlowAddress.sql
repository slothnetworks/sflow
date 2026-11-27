-- goflow2 v2 sends addresses as raw bytes: 4 bytes for IPv4, 16 for IPv6, empty when absent.
-- IPv4 is mapped to ::ffff:a.b.c.d. (The old version guessed IPv4 from "the low 12 bytes are zero",
-- which misread IPv6 addresses like 2001:db8::.)
--
-- Must never throw: an exception in flows_raw_view stops the Kafka consumer on that message.
-- multiIf may evaluate every branch for the whole block, so toFixedString is applied only to
-- its result, which is always exactly 16 bytes (anything that isn't 4 or 16 bytes gives ::).
CREATE OR REPLACE FUNCTION ParseGoFlowAddress AS (Address) ->
    CAST(toFixedString(multiIf(
        length(Address) = 4,  concat(repeat(unhex('00'), 10), unhex('FFFF'), Address),
        length(Address) = 16, Address,
        repeat(unhex('00'), 16)
    ), 16), 'IPv6');
