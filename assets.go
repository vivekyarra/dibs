// Package dibs holds the files that ship inside the dibs binary: the Switch
// template source and the agent instructions it includes.
package dibs

import "embed"

// Assets holds switch/ (the template source and its generated output) and
// agents/ (the DIBS PROTOCOL and each agent's instructions).
//
//go:embed switch agents
var Assets embed.FS
