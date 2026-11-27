//go:build tools

package NetMeta

// Static imports for command-line dependencies we build from source.

import (
	_ "cuelang.org/go/cmd/cue"
	_ "github.com/netsampler/goflow2/v2/cmd/goflow2"
	_ "github.com/pressly/goose/v3/cmd/goose"
)
