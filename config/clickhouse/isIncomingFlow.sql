-- isIncomingFlow decides which flows the dashboards count as "in" and which as "out".
-- cmd/migrate re-applies this file every time the stack starts, so edit it here and restart
-- the `migrate` service:   docker compose up -d --force-recreate migrate
--
-- The default trusts the exporter's flow direction (0 = ingress). sFlow has no direction
-- field, so with sFlow every flow counts as "in". To fix that, add per-sampler rules
-- that return 1/0 for flows you can classify, or NULL to fall through to the next rule:
--
--   CREATE OR REPLACE FUNCTION isIncomingFlow AS
--       (SamplerAddress, SrcAddr, DstAddr, SrcAS, DstAS, FlowDirection) ->
--   coalesce(
--       -- sampler ::ffff:192.0.2.1: traffic towards our own prefix is incoming
--       if(toString(SamplerAddress) = '::ffff:192.0.2.1',
--          isIPAddressInRange(toString(DstAddr), toIPv6Net('198.51.100.0/24')), NULL),
--       -- sampler 2001:db8::1: anything not sourced from our ASN is incoming
--       if(toString(SamplerAddress) = '2001:db8::1', SrcAS != 64496, NULL),
--       FlowDirection = 0
--   );
--
-- The signature (argument names and order) must stay the same: dashboard queries call it.

CREATE OR REPLACE FUNCTION isIncomingFlow AS
    (SamplerAddress, SrcAddr, DstAddr, SrcAS, DstAS, FlowDirection) ->
coalesce(FlowDirection = 0);
