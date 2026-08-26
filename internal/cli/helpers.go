package cli

import (
	"errors"

	"github.com/google/uuid"
)

// uuidOf parses an id the API already validated. A malformed one cannot reach
// here -- it came out of a response -- so a parse failure yields the zero UUID
// and the call it feeds fails with the API's own 404 rather than a panic.
func uuidOf(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// errorsAs is errors.As, named so the Windows build can use it without an
// import that the unix build would flag as unused.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
