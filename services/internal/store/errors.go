package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConflictError is a write the database refused because it would break one of
// the schema's rules — two trunks carrying the same number, a line outside its
// trunk's numbers, a slug already taken.
//
// Message is written for the API caller and safe to return. Everything else
// about a database error stays in the log: a raw Postgres message leaks column
// and constraint names, and sometimes row contents.
type ConflictError struct {
	Message string
	err     error
}

func (e *ConflictError) Error() string { return "store: " + e.Message }
func (e *ConflictError) Unwrap() error { return e.err }

// sqlstateRule is what the schema's triggers raise. Their messages are written
// for the caller, so they pass through as they are.
const sqlstateRule = "IN001"

// constraintMessages turns a named constraint into something a caller can act
// on. A constraint that is not listed still becomes a conflict, with a generic
// message rather than its name.
var constraintMessages = map[string]string{
	"trunks_numbers_no_overlap": "some of these numbers are already carried by another trunk; " +
		"a number can arrive on only one trunk",
	"traffic_configs_numbers_no_overlap": "another traffic config already claims some of these " +
		"numbers in this direction",
	"traffic_configs_not_empty": "did_ranges must contain at least one number",
	"trunks_slug_idx":           "a trunk with this slug already exists",
	"tenants_pkey":              "a tenant with this id already exists",
}

// classify converts the database errors a caller caused into ConflictError,
// and leaves everything else alone.
func classify(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}

	switch pg.Code {
	case sqlstateRule:
		return &ConflictError{Message: pg.Message, err: err}

	case "23P01", "23505", "23514": // exclusion, unique, check
		msg, ok := constraintMessages[pg.ConstraintName]
		if !ok {
			msg = "this change conflicts with existing configuration"
		}
		return &ConflictError{Message: msg, err: err}

	case "23503": // foreign key
		return &ConflictError{Message: "this refers to something that does not exist", err: err}
	}

	return err
}
