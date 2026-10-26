// Package autobuilder holds the agent's command, so a runner can build a
// tenant repo that doesn't carry it (TENANCY.md).
package autobuilder

import _ "embed"

// BuildBoard is .claude/commands/build-board.md.
//
//go:embed .claude/commands/build-board.md
var BuildBoard string
