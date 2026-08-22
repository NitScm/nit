package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NitScm/nit/pkg/policy"
)

// Bundle file names, relative to the bundle root.
const (
	usersFile        = "users.yaml"
	groupsFile       = "groups.yaml"
	repositoriesFile = "repositories.yaml"
	repositoriesDir  = "repositories"
	rulesFile        = "rules.yaml"
)

// Load reads a policy bundle from a directory and compiles it.
func Load(root string) (*policy.Policy, error) {
	return LoadFS(os.DirFS(root))
}

// LoadFS reads a policy bundle from a file system. Taking an fs.FS rather than
// a path keeps the loader testable without touching disk, and leaves the door
// open to bundles served from an embedded FS or fetched from a git object
// store.
func LoadFS(fsys fs.FS) (*policy.Policy, error) {
	version, err := hashBundle(fsys)
	if err != nil {
		return nil, err
	}

	spec := policy.Spec{
		Tenant:  policy.DefaultTenant,
		Version: version,
		Rules:   map[policy.RepoID][]policy.Rule{},
	}

	var users []UserFile
	if err := decodeFile(fsys, usersFile, &users); err != nil {
		return nil, err
	}
	for _, u := range users {
		spec.Users = append(spec.Users, policy.User{
			ID:          policy.UserID(u.ID),
			Email:       u.Email,
			Aliases:     u.Aliases,
			ForgeLogins: u.ForgeLogins,
			Disabled:    u.Disabled,
		})
	}

	var groups []GroupFile
	if err := decodeFile(fsys, groupsFile, &groups); err != nil {
		return nil, err
	}
	for _, g := range groups {
		group := policy.Group{
			ID:          policy.GroupID(g.ID),
			Description: g.Description,
		}
		for _, m := range g.Members {
			group.Members = append(group.Members, policy.UserID(m))
		}
		for _, inc := range g.Includes {
			group.Includes = append(group.Includes, policy.GroupID(inc))
		}
		spec.Groups = append(spec.Groups, group)
	}

	var repos []RepositoryFile
	if err := decodeFile(fsys, repositoriesFile, &repos); err != nil {
		return nil, err
	}
	for _, r := range repos {
		spec.Repositories = append(spec.Repositories, policy.Repository{
			ID:            policy.RepoID(r.ID),
			Remote:        r.Remote,
			Forge:         r.Forge,
			DefaultBranch: r.DefaultBranch,
		})
	}

	for _, r := range repos {
		rules, err := loadRules(fsys, r.ID)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			spec.Rules[policy.RepoID(r.ID)] = rules
		}
	}

	return policy.Compile(spec)
}

func loadRules(fsys fs.FS, repoID string) ([]policy.Rule, error) {
	name := path.Join(repositoriesDir, repoID, rulesFile)

	var files []RuleFile
	if err := decodeFile(fsys, name, &files); err != nil {
		return nil, err
	}

	rules := make([]policy.Rule, 0, len(files))

	for i, rf := range files {
		id := rf.ID
		if id == "" {
			id = fmt.Sprintf("%s#%d", repoID, i)
		}

		rule := policy.Rule{
			ID:          id,
			Effect:      policy.Effect(rf.Effect),
			Description: rf.Description,
			Subject: policy.RuleSubject{
				Type: policy.SubjectType(rf.Subject.Type),
				ID:   rf.Subject.ID,
			},
		}

		for _, ex := range rf.Except {
			rule.Except = append(rule.Except, policy.RuleSubject{
				Type: policy.SubjectType(ex.Type),
				ID:   ex.ID,
			})
		}

		for _, p := range rf.Paths {
			pattern, err := policy.ParsePattern(p)
			if err != nil {
				return nil, fmt.Errorf("%s: rule %s: %w", name, id, err)
			}
			rule.Paths = append(rule.Paths, pattern)
		}

		for _, r := range rf.Refs {
			pattern, err := policy.ParsePattern(r)
			if err != nil {
				return nil, fmt.Errorf("%s: rule %s: refs: %w", name, id, err)
			}
			rule.Refs = append(rule.Refs, pattern)
		}

		for _, a := range rf.Actions {
			rule.Actions = append(rule.Actions, policy.Action(a))
		}

		rules = append(rules, rule)
	}

	return rules, nil
}

// decodeFile decodes a YAML document into out. A missing file is not an error:
// a bundle may legitimately declare no group at all. Unknown fields are
// rejected.
func decodeFile(fsys fs.FS, name string, out any) error {
	f, err := fsys.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("policy: open %s: %w", name, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	if err := dec.Decode(out); err != nil {
		// An empty file decodes to io.EOF; treat it as "nothing declared".
		if errors.Is(err, fs.ErrClosed) || err.Error() == "EOF" {
			return nil
		}
		return fmt.Errorf("policy: parse %s: %w", name, err)
	}

	return nil
}

// hashBundle computes a stable content hash over every YAML file of the bundle.
//
// The hash covers file names as well as contents, so that adding, removing or
// renaming a file changes the version even when the rules happen to hash the
// same.
func hashBundle(fsys fs.FS) (string, error) {
	var names []string

	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		names = append(names, name)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("policy: walk bundle: %w", err)
	}

	sort.Strings(names)

	h := sha256.New()

	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return "", fmt.Errorf("policy: read %s: %w", name, err)
		}

		fmt.Fprintf(h, "%s\x00%d\x00", name, len(content))
		h.Write(content)
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16], nil
}
