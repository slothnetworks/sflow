-- Renders ::ffff:a.b.c.d as a.b.c.d and everything else as IPv6.
CREATE OR REPLACE FUNCTION IPv6ToString AS (Address) ->
    if(startsWith(reinterpret(Address, 'FixedString(16)'), concat(repeat(unhex('00'), 10), repeat(unhex('FF'), 2))),
       IPv4NumToString(CAST(reinterpret(reverse(substring(reinterpret(Address, 'FixedString(16)'), 13, 16)), 'UInt32'), 'IPv4')),
       IPv6NumToString(Address));
