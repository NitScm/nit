package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/audit/audittest"
)

func TestMemoryConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) (audit.Sink, audittest.Readback) {
		sink := audit.NewMemory()

		return sink, func(t *testing.T, _ int) []audit.Record { return sink.Records() }
	})
}

// Multi is the sink an operator ends up with the moment a deployment exports to
// more than one destination, so it is held to the same contract as one.
func TestMultiConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) (audit.Sink, audittest.Readback) {
		first, second := audit.NewMemory(), audit.NewMemory()

		return audit.Multi{first, nil, second}, func(t *testing.T, _ int) []audit.Record {
			// Read from the second, so a Multi that stopped after the first
			// sink fails here rather than passing on the first one's records.
			return second.Records()
		}
	})
}

func TestMultiAttemptsEverySinkAndJoinsTheErrors(t *testing.T) {
	failing := audit.NewMemory()
	failing.Fail(errors.New("destination unreachable"))

	working := audit.NewMemory()

	err := audit.Multi{failing, working}.Emit(context.Background(),
		audit.Record{Action: "push.accepted"})

	if err == nil {
		t.Fatal("Emit reported success though a sink failed")
	}

	// The point of joining rather than returning early: the order sinks were
	// configured in must not decide which destinations receive a record.
	if working.Len() != 1 {
		t.Errorf("the working sink received %d records, want 1: a failure earlier "+
			"in the list stopped the fan-out", working.Len())
	}
}

func TestFailingMemoryRecordsNothing(t *testing.T) {
	sink := audit.NewMemory()
	sink.Fail(errors.New("nope"))

	if err := sink.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err == nil {
		t.Fatal("Emit succeeded though the sink was told to fail")
	}

	if sink.Len() != 0 {
		t.Errorf("a failing sink kept %d records", sink.Len())
	}

	sink.Fail(nil)

	if err := sink.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err != nil {
		t.Fatalf("Emit after clearing the failure: %v", err)
	}

	if sink.Len() != 1 {
		t.Errorf("Len = %d, want 1 after the failure was cleared", sink.Len())
	}
}
