// Package migrations embeds the versioned SQL schema migrations.
//
// Files are named NNNN_description.sql and applied in order. A migration that
// has shipped must never be edited; add a new one instead (the runner verifies
// checksums and refuses to start if an applied migration changed).
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
