package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NitScm/nit/pkg/policy"
)

// ErrTestsInsideBundle is a tests file living in the bundle it tests.
//
// Refused rather than tolerated. The bundle's version is a SHA-256 over every
// YAML file in its directory, and that version is stamped on every decision and
// every audit record — so a tests file inside it would make editing a test
// change the identity of the rules. A decision from last month would appear to
// have come from a bundle nobody can reconstruct, and the failure would be
// invisible until somebody tried.
var ErrTestsInsideBundle = errors.New("policy: the expectations file is inside the bundle it tests")

// LoadExpectations reads a file of expectations.
//
// Strict decoding, like the bundle itself: an unknown field is an error rather
// than a shrug. A misspelled `expect:` that silently defaulted would turn a
// test into decoration, which is worse than having no test — nobody audits a
// green check.
func LoadExpectations(bundle, path string) ([]policy.Expectation, error) {
	if inside, err := within(bundle, path); err != nil {
		return nil, err
	} else if inside {
		return nil, fmt.Errorf("%w: %s is under %s. Put it beside the bundle instead",
			ErrTestsInsideBundle, path, bundle)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("policy: open expectations: %w", err)
	}

	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var out []policy.Expectation

	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("policy: read %s: %w", path, err)
	}

	if len(out) == 0 {
		// An empty file passes every run and asserts nothing. Somebody wrote it
		// meaning to fill it in.
		return nil, fmt.Errorf("policy: %s has no expectations in it", path)
	}

	return out, nil
}

func within(bundle, path string) (bool, error) {
	root, err := filepath.Abs(bundle)
	if err != nil {
		return false, fmt.Errorf("policy: %w", err)
	}

	file, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("policy: %w", err)
	}

	rel, err := filepath.Rel(root, file)
	if err != nil {
		return false, nil
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}
