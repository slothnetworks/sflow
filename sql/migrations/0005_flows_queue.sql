-- ClickHouse has no DDL transactions.
-- +goose NO TRANSACTION

-- Kafka consumer for goflow2 v2 (-format=bin, varint length-delimited protobuf).
-- Column names match config/clickhouse/format_schemas/flow.proto field names.
-- kafka_handle_error_mode='stream' keeps one malformed message from stalling ingestion;
-- the view drops rows where _error is set.

-- +goose Up
CREATE TABLE IF NOT EXISTS flows_queue
(
    type               Enum8('FLOWUNKNOWN' = 0, 'SFLOW_5' = 1, 'NETFLOW_V5' = 2, 'NETFLOW_V9' = 3, 'IPFIX' = 4),
    time_received_ns   UInt64,
    sequence_num       UInt32,
    sampling_rate      UInt64,
    sampler_address    String,
    time_flow_start_ns UInt64,
    time_flow_end_ns   UInt64,
    bytes              UInt64,
    packets            UInt64,
    src_addr           String,
    dst_addr           String,
    etype              UInt32,
    proto              UInt32,
    src_port           UInt32,
    dst_port           UInt32,
    in_if              UInt32,
    out_if             UInt32,
    src_mac            UInt64,
    dst_mac            UInt64,
    src_vlan           UInt32,
    dst_vlan           UInt32,
    vlan_id            UInt32,
    ip_tos             UInt32,
    forwarding_status  UInt32,
    ip_ttl             UInt32,
    tcp_flags          UInt32,
    icmp_type          UInt32,
    icmp_code          UInt32,
    ipv6_flow_label    UInt32,
    fragment_id        UInt32,
    fragment_offset    UInt32,
    src_as             UInt32,
    dst_as             UInt32,
    next_hop           String,
    next_hop_as        UInt32,
    src_net            UInt32,
    dst_net            UInt32,
    ingress_vrf_id     UInt32,
    egress_vrf_id      UInt32,
    bi_flow_direction  UInt32,
    flow_direction     UInt32
) ENGINE = Kafka
SETTINGS
    kafka_broker_list = 'kafka:9092',
    kafka_topic_list = 'flow-messages',
    kafka_group_name = 'clickhouse',
    kafka_format = 'Protobuf',
    kafka_schema = 'flow.proto:FlowMessage',
    kafka_max_block_size = 1048576,
    kafka_handle_error_mode = 'stream';

-- +goose Down
DROP TABLE IF EXISTS flows_queue;
