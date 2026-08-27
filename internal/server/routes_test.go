package server_test

import (
	"testing"

	"github.com/NitScm/nit/pkg/protocol"
)

// What the server registers must be exactly what protocol.Routes says.
//
// This is the test that lets the API description live outside this module. The
// description is compared against protocol.Routes by whoever holds it, and that
// comparison is only worth anything if this list is the truth about what is
// served. Without this, the exported list becomes a comment that drifts.
func TestTheExportedRoutesAreTheRegisteredOnes(t *testing.T) {
	f := newFixture(t)

	registered := map[string]bool{}
	for _, route := range f.server.Routes() {
		registered[route] = true
	}

	exported := map[string]bool{}
	for _, route := range protocol.Routes() {
		exported[route] = true
	}

	for route := range registered {
		if !exported[route] {
			t.Errorf("%s is served and not in protocol.Routes. Add it there, or the "+
				"API description cannot know about it", route)
		}
	}

	for route := range exported {
		if !registered[route] {
			t.Errorf("%s is in protocol.Routes and not served. Anything generated "+
				"from that list calls a route that does not exist", route)
		}
	}

	if len(exported) == 0 {
		t.Fatal("protocol.Routes is empty; this test proves nothing")
	}
}
