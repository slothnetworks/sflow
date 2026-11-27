CREATE OR REPLACE FUNCTION VLANToString AS (Sampler, VLAN) ->
    dictGetStringOrDefault('VlanNames', 'Description', (IPv6NumToString(Sampler), VLAN), VLAN);
