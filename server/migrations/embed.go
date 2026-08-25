// Package migrations embeds the SQL schema history.
//
// The embed lives here rather than in the db package because go:embed cannot
// traverse upward with "..": patterns resolve relative to the source file's
// own directory. Keeping the SQL beside the embed also means a migration file
// added to this directory is picked up with no other change.
package migrations

import "embed"

// FS holds every *.up.sql and *.down.sql in this directory.
//
//go:embed *.sql
var FS embed.FS
