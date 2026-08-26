package server

import (
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// The group view answers "who is in this group, in the bundle the server is
// serving". That is a different question from the one groups.yaml answers, and
// where membership comes from a directory read at run time it is the only place
// the right one is asked — so effective membership, not the list in the file.
func TestGroupViewsResolveMembershipThroughIncludes(t *testing.T) {
	p, err := policy.Compile(policy.Spec{
		Version: "v1",
		Users: []policy.User{
			{ID: "break-glass"},
			{ID: "omar@corp.example"},
		},
		Groups: []policy.Group{
			{ID: "idp:payments", Members: []policy.UserID{"omar@corp.example"}},
			{
				ID:       "payments",
				Members:  []policy.UserID{"break-glass"},
				Includes: []policy.GroupID{"idp:payments"},
			},
			{ID: "nobody"},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	views := map[string]PolicyGroupView{}
	for _, v := range groupViews(p) {
		views[v.ID] = v
	}

	payments := views["payments"]

	if len(payments.Members) != 2 ||
		payments.Members[0] != "break-glass" ||
		payments.Members[1] != "omar@corp.example" {
		t.Fatalf("payments members = %v; the included group's member is missing", payments.Members)
	}

	if len(payments.Includes) != 1 || payments.Includes[0] != "idp:payments" {
		t.Fatalf("payments includes = %v", payments.Includes)
	}

	// An empty group is empty, not absent: a directory group nothing supplied
	// has to be visible, because "nobody is in it" is the answer somebody is
	// looking for when access disappeared.
	empty, ok := views["nobody"]
	if !ok {
		t.Fatal("a group with no members is not in the view at all")
	}

	if empty.Members == nil {
		t.Fatal("an empty group's members are null rather than an empty list")
	}

	if len(empty.Members) != 0 {
		t.Fatalf("nobody members = %v", empty.Members)
	}
}
