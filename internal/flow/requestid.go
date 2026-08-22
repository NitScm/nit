package flow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newRequestID generates the idempotency key of a submission.
//
// It has to be unguessable and unique per attempt, not per retry: the whole
// point is that resending the *same* request id returns the original task
// instead of creating a second upstream commit. A counter would collide across
// machines; a timestamp would collide within a millisecond.
func newRequestID() string {
	var buf [16]byte

	if _, err := rand.Read(buf[:]); err != nil {
		// Without entropy there is no safe way to make a request idempotent,
		// and a colliding id would silently return somebody else's task.
		panic(fmt.Sprintf("nit: entropy unavailable: %v", err))
	}

	return hex.EncodeToString(buf[:])
}
