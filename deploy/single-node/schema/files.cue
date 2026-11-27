@extern(embed)

package schema

file: "DBManager.proto": _ @embed(file=DBManager.proto,type=text)

file: "FlowMessage.proto": _ @embed(file=FlowMessage.proto,type=text)

// FastNetMon traffic format
// https://github.com/pavel-odintsov/fastnetmon/blob/master/src/traffic_output_formats/protobuf/traffic_data.proto
file: "traffic_data.proto": _ @embed(file=traffic_data.proto,type=text)