CREATE OR REPLACE FUNCTION HostToString AS (Sampler, Host) ->
    dictGetStringOrDefault('HostNames', 'Description', (IPv6NumToString(Sampler), Host), IPv6ToString(Host));
