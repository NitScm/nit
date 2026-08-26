package config_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
)

func TestABundleMayNotDeclareADirectoryGroup(t *testing.T) {
	fsys := bundleFS("allow")
	fsys["groups.yaml"] = &fstest.MapFile{
		Data: []byte("- id: devs\n  members: [dev]\n\n- id: idp:devs\n  members: [dev]\n"),
	}

	_, err := policyconfig.LoadFS(fsys)
	if !errors.Is(err, policy.ErrReservedGroupPrefix) {
		t.Fatalf("got %v, want ErrReservedGroupPrefix", err)
	}

	// The person who has to fix it is looking at a file, so the error names it.
	if !strings.Contains(err.Error(), "groups.yaml") {
		t.Fatalf("the error does not say which file to open: %v", err)
	}
}

// The refusal is about declaring one. Referring to one is the ordinary case and
// has to load, with or without anything to supply it.
func TestABundleMayIncludeADirectoryGroup(t *testing.T) {
	fsys := bundleFS("allow")
	fsys["groups.yaml"] = &fstest.MapFile{
		Data: []byte("- id: devs\n  members: [dev]\n  includes: [idp:devs]\n"),
	}

	p, err := policyconfig.LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}

	for _, g := range p.Groups() {
		if g.ID == "idp:devs" && len(g.Members) == 0 {
			return
		}
	}

	t.Fatal("the included directory group is not there, or is not empty")
}
