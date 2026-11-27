package schema

#Config: {
	fastNetMon?: {
		...
	}
	sampler: [DEVICE=string]: isIncomingFlow: [...{[COLUMN=string]: string | int}]
}

// Function contains all information to create or benchmark a UDF inside Clickhouse
function: [NAME=string]: {
	name: NAME
	arguments: [...string]
	query: string
}

// MaterializedView contains all information to create a MaterializedView inside Clickhouse
// It also allows to benchmark the select statement inside the MV
view: [NAME=string]: {
	name:  NAME
	to:    string
	from:  string
	query: string
}

// Table contains the Name, the Type, additional Settings and
// a reference to a Message inside the Protobuf file
table: [NAME=string]:{
	name:     NAME
	schema:   string
	engine:   string
	settings: [string]: _
}
