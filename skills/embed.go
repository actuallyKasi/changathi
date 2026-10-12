// Package skills embeds the agent skills that ship with the changathi binary,
// so an installed skill always matches the CLI that installed it.
package skills

import _ "embed"

//go:embed changathi/SKILL.md
var Changathi string
