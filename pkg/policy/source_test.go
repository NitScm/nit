package policy_test

import (
	"testing"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/policy/policytest"
)

// Static is the simplest possible Source, and running the suite against it is
// what proves the suite is about the contract rather than about the directory
// loader that happened to be written first.
func TestStaticConformance(t *testing.T) {
	policytest.Run(t, func(t *testing.T) policytest.Harness {
		p, err := policy.Compile(policytest.Bundle("static-1", true))
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}

		return policytest.Harness{Source: policy.Static{Policy: p}}
	})
}
