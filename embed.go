// Package ka holds the files the ka binary embeds from the repository root.
package ka

import "embed"

// UI is the built web UI (web/, built by `npm run build` into ui/). The
// daemon serves a checkout's ui/ when one is set up and matches this
// binary's API, and these embedded files otherwise, so it works offline.
//
//go:embed all:ui
var UI embed.FS

// Skill is the agent skill `ka setup` installs and `ka skill` prints.
//
//go:embed .agents/skills/ka/SKILL.md
var Skill string
