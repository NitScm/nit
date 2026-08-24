package audit

import (
	"context"
	"slices"
	"sync"
)

// Memory is a Sink that keeps records in a slice.
//
// It exists for the same reason policy.Static and blob.Memory do: anything
// written against one of these seams needs a working one to test against, and
// "a sink that records what it received" is otherwise forty lines every
// consumer writes for themselves. Each of those copies is a chance to keep the
// caller's slice, or to lose a record under a concurrent emit, and then to
// bless the mistake in a test.
//
// It can also be told to fail. The package promises that a failed sink write
// never fails the operation being recorded, and a promise nobody can exercise
// is one that quietly stops being true.
//
// Not for production: everything is resident and a restart loses it.
//
// It passes pkg/audit/audittest.
type Memory struct {
	mu      sync.Mutex
	records []Record
	err     error
}

// NewMemory returns an empty sink.
func NewMemory() *Memory { return &Memory{} }

// Emit implements Sink.
//
// The records are copied. A sink that retained the caller's slice would share
// memory with code that is free to reuse it, and the result is records
// attributed to the wrong person — invisible in the sink's own tests.
func (m *Memory) Emit(_ context.Context, records ...Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return m.err
	}

	m.records = append(m.records, records...)

	return nil
}

// Records returns what has been received, in order, as a copy.
func (m *Memory) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.records)
}

// Len reports how many records have arrived, without copying them.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.records)
}

// Fail makes every subsequent Emit return err, and records nothing. Passing nil
// restores ordinary behaviour.
func (m *Memory) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.err = err
}

// Reset forgets everything received, leaving any configured failure in place.
func (m *Memory) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.records = nil
}

var _ Sink = (*Memory)(nil)
