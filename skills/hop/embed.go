// Package hopskill embeds the distributable Hop agent skill in the CLI binary.
package hopskill

import "embed"

// Files contains the complete vendor-neutral Hop skill bundle.
//
//go:embed SKILL.md agents/openai.yaml references/protocol.md
var Files embed.FS
