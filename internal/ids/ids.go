// Package ids makes the IDs of runplane rows.
package ids

import (
	"strings"

	"github.com/oklog/ulid/v2"
)

// New returns a new lowercase ULID. ULIDs sort by time.
func New() string {
	return strings.ToLower(ulid.Make().String())
}
