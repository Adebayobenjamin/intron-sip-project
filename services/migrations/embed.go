// Package migrations holds the schema as embedded SQL.
//
// Embedded rather than read from disk. The runtime image is distroless — no
// shell, no package manager, and no migrations directory unless something puts
// one there. Embedding makes the binary and its schema one artifact, which is
// also what makes an immutable image tag mean something: the tag names the
// code AND the schema that code expects.
//
// The files live here rather than inside internal/store because go:embed
// cannot reach outside its own package directory, and the schema is worth
// having somewhere obvious.
package migrations

import "embed"

// FS holds every .sql file in this directory.
//
//go:embed *.sql
var FS embed.FS

// Version is the migration this build expects.
//
// Bumped by hand alongside a new migration file. esl-controller checks it at
// startup and refuses to run against a database control-plane has not migrated
// yet — deploy order is control-plane first, and this makes getting it wrong
// loud rather than subtle.
const Version = 1
