package config_test

import (
	"testing"
	"testing/fstest"

	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
)

func bundleFS(effect string) fstest.MapFS {
	return fstest.MapFS{
		"users.yaml":  {Data: []byte("- id: dev\n  email: dev@example.com\n")},
		"groups.yaml": {Data: []byte("- id: devs\n  members: [dev]\n")},
		"repositories.yaml": {Data: []byte(
			"- id: repo\n  remote: https://example.com/r.git\n  forge: github\n  default_branch: main\n")},
		"repositories/repo/rules.yaml": {Data: []byte(
			"- id: devs-read\n  subject: { type: group, id: devs }\n  paths: [\"**\"]\n  actions: [read]\n  effect: " + effect + "\n")},
	}
}

// LoadSpecFS reads the same bundle LoadFS reads and stops one step earlier.
// Composing on top of it must therefore be indistinguishable from loading it.
func TestLoadSpecFSMatchesLoadFS(t *testing.T) {
	fsys := bundleFS("allow")

	spec, err := policyconfig.LoadSpecFS(fsys)
	if err != nil {
		t.Fatalf("LoadSpecFS: %v", err)
	}

	fromSpec, err := policy.Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	direct, err := policyconfig.LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}

	if fromSpec.Version() != direct.Version() {
		t.Errorf("versions differ: %s and %s", fromSpec.Version(), direct.Version())
	}

	subject, err := fromSpec.Subject("dev")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	request := policy.Request{
		Repo: "repo", Ref: "refs/heads/main", Subject: subject,
		Path: "src/app.go", Action: policy.ActionRead,
	}

	if fromSpec.Evaluate(request).Allowed != direct.Evaluate(request).Allowed {
		t.Error("the two paths decide differently")
	}
}

// The case the function exists for: membership merged from somewhere else,
// before compilation, without re-implementing the reader.
func TestASpecCanBeComposedBeforeCompiling(t *testing.T) {
	spec, err := policyconfig.LoadSpecFS(bundleFS("allow"))
	if err != nil {
		t.Fatalf("LoadSpecFS: %v", err)
	}

	// A user the files do not know, put into a group the files declare —
	// exactly what a directory merge does.
	spec.Users = append(spec.Users, policy.User{ID: "from-directory", Email: "d@example.com"})

	for i := range spec.Groups {
		if spec.Groups[i].ID == "devs" {
			spec.Groups[i].Members = append(spec.Groups[i].Members, "from-directory")
		}
	}

	// A composed bundle sets its own version: it is no longer the one the hash
	// describes, and everything keyed on a version would otherwise conflate
	// the two.
	spec.Version = "composed-1"

	p, err := policy.Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	subject, err := p.Subject("from-directory")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	decision := p.Evaluate(policy.Request{
		Repo: "repo", Ref: "refs/heads/main", Subject: subject,
		Path: "src/app.go", Action: policy.ActionRead,
	})

	if !decision.Allowed {
		t.Error("a user merged into a group did not inherit the group's rule")
	}
}

// Compile is not optional, and a Spec that skips it is not a policy: a rule
// naming a group nobody declares has to be refused, not served.
func TestCompileStillValidatesAComposedSpec(t *testing.T) {
	spec, err := policyconfig.LoadSpecFS(bundleFS("allow"))
	if err != nil {
		t.Fatalf("LoadSpecFS: %v", err)
	}

	spec.Rules["repo"] = append(spec.Rules["repo"], policy.Rule{
		ID:      "dangling",
		Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "nobody-declares-this"},
		Paths:   []policy.Pattern{policy.MustParsePattern("**")},
		Actions: []policy.Action{policy.ActionRead},
		Effect:  policy.EffectAllow,
	})

	if _, err := policy.Compile(spec); err == nil {
		t.Fatal("Compile accepted a rule naming an undeclared group")
	}
}
