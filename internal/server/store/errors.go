package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Typed error taxonomy shared by every store file. Sentinels are matched
// with errors.Is; each constructor keeps the call site's exact
// human-readable message (the CLI prints these verbatim).

// ErrNotFound marks admin lookups that resolved nothing; httpapi maps it to
// 404 without string matching. Match with errors.Is.
var ErrNotFound = errors.New("not found")

// notFoundError keeps each site's exact human-readable message (the CLI
// prints these verbatim) while still matching errors.Is(err, ErrNotFound).
type notFoundError struct{ msg string }

func (e notFoundError) Error() string        { return e.msg }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

func notFoundf(format string, args ...any) error {
	return notFoundError{msg: fmt.Sprintf(format, args...)}
}

// ErrInvalid marks admin writes that can never succeed as requested;
// httpapi maps it to 400.
var ErrInvalid = errors.New("invalid")

type invalidError struct{ msg string }

func (e invalidError) Error() string        { return e.msg }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

func invalidf(format string, args ...any) error {
	return invalidError{msg: fmt.Sprintf(format, args...)}
}

// ErrConflict marks admin writes refused because they collide with an
// existing row of a different kind; httpapi maps it to 409.
var ErrConflict = errors.New("conflict")

type conflictError struct{ msg string }

func (e conflictError) Error() string        { return e.msg }
func (e conflictError) Is(target error) bool { return target == ErrConflict }

func conflictf(format string, args ...any) error {
	return conflictError{msg: fmt.Sprintf(format, args...)}
}

// isFKViolation reports whether err is a PostgreSQL foreign-key violation
// (SQLSTATE 23503). Admin writers resolve names to ids before inserting, so
// a parent row deleted in between (possible since sites became deletable)
// surfaces here — callers translate it to a typed error instead of letting
// it escape as an opaque 500.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// InUseError reports a target delete blocked by referencing probe configs;
// httpapi maps it to 409.
type InUseError struct {
	Name  string
	Count int64
}

func (e InUseError) Error() string {
	return fmt.Sprintf("target %q is referenced by %d probe config(s)", e.Name, e.Count)
}
