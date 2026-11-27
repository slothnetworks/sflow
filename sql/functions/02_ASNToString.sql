CREATE OR REPLACE FUNCTION ASNToString AS (ASN) ->
    concat(substring(dictGetString('autnums', 'name', toUInt64(ASN)), 1, 25), ' AS', toString(ASN));
