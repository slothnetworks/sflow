-- ClickHouse has no DDL transactions.
-- +goose NO TRANSACTION

-- Moves flows from the Kafka queue into flows_raw.
--
-- Columns are mapped explicitly. The previous deployment used SELECT *, so the proto field
-- `Etype` never matched the flows_raw column `EType` and EType was always 0.
--
-- Depends on the UDF ParseGoFlowAddress (sql/functions); cmd/migrate creates functions first.

-- +goose Up
CREATE MATERIALIZED VIEW IF NOT EXISTS flows_raw_view TO flows_raw AS
WITH
    ParseGoFlowAddress(sampler_address) AS sampler,
    IPv6NumToString(sampler) AS sampler_key,
    dictGetOrDefault('SamplerConfig', 'AnonymizeAddresses', sampler_key, false) AS anonymize,
    ParseGoFlowAddress(src_addr) AS src,
    ParseGoFlowAddress(dst_addr) AS dst,
    intDiv(time_received_ns, 1000000000) AS received
SELECT
    toDate(received)                                                               AS Date,
    type                                                                           AS FlowType,
    sequence_num                                                                   AS SequenceNum,
    received                                                                       AS TimeReceived,
    coalesce(dictGetOrNull('SamplerConfig', 'SamplingRate', sampler_key), sampling_rate) AS SamplingRate,
    toUInt8(flow_direction)                                                        AS FlowDirection,
    sampler                                                                        AS SamplerAddress,
    intDiv(time_flow_start_ns, 1000000000)                                         AS TimeFlowStart,
    intDiv(time_flow_end_ns, 1000000000)                                           AS TimeFlowEnd,
    bytes                                                                          AS Bytes,
    packets                                                                        AS Packets,
    if(anonymize, toIPv6(cutIPv6(src, 8, 1)), src)                                 AS SrcAddr,
    if(anonymize, toIPv6(cutIPv6(dst, 8, 1)), dst)                                 AS DstAddr,
    toUInt16(etype)                                                                AS EType,
    toUInt8(proto)                                                                 AS Proto,
    src_port                                                                       AS SrcPort,
    dst_port                                                                       AS DstPort,
    in_if                                                                          AS InIf,
    out_if                                                                         AS OutIf,
    src_mac                                                                        AS SrcMac,
    dst_mac                                                                        AS DstMac,
    src_vlan                                                                       AS SrcVlan,
    dst_vlan                                                                       AS DstVlan,
    vlan_id                                                                        AS VlanId,
    ingress_vrf_id                                                                 AS IngressVrfId,
    egress_vrf_id                                                                  AS EgressVrfId,
    toUInt8(ip_tos)                                                                AS IPTos,
    toUInt8(forwarding_status)                                                     AS ForwardingStatus,
    toUInt8(ip_ttl)                                                                AS IPTTL,
    toUInt8(tcp_flags)                                                             AS TCPFlags,
    toUInt8(icmp_type)                                                             AS IcmpType,
    toUInt8(icmp_code)                                                             AS IcmpCode,
    ipv6_flow_label                                                                AS IPv6FlowLabel,
    fragment_id                                                                    AS FragmentId,
    fragment_offset                                                                AS FragmentOffset,
    toUInt8(bi_flow_direction)                                                     AS BiFlowDirection,
    -- AS lookups use the unanonymised address
    if(src_as = 0, dictGetOrDefault('risinfo', 'asnum', src, toUInt32(0)), src_as) AS SrcAS,
    if(dst_as = 0, dictGetOrDefault('risinfo', 'asnum', dst, toUInt32(0)), dst_as) AS DstAS,
    if(length(next_hop) = 0, toIPv6('::'), ParseGoFlowAddress(next_hop))           AS NextHop,
    next_hop_as                                                                    AS NextHopAS,
    toUInt8(src_net)                                                               AS SrcNet,
    toUInt8(dst_net)                                                               AS DstNet
FROM flows_queue
WHERE length(_error) = 0;

-- +goose Down
DROP VIEW IF EXISTS flows_raw_view;
