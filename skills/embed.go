// Package skills embeds the CLI instructions shipped with Control.
package skills

import "embed"

//go:embed control/SKILL.md
var Control []byte

// ControlFiles includes the core skill and its on-demand references under control/.
//
//go:embed control/SKILL.md control/references/*.md
var ControlFiles embed.FS
