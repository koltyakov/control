// Package skills embeds the CLI instructions shipped with Control.
package skills

import _ "embed"

//go:embed control/SKILL.md
var Control []byte
