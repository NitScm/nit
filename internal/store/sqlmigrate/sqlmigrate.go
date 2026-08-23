// Package sqlmigrate loads versioned schema files.
//
// Dialect-neutral on purpose: it parses file names and pairs up and down, and
// knows nothing about SQL. Each backend applies what this returns in the way
// its engine requires — the locking and transaction rules around a migration
// differ between PostgreSQL and MySQL far more than the parsing does.
package sqlmigrate

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// Migration is one schema version.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Load reads migrations from a file system, expecting names of the
// form "0001_init.up.sql" and "0001_init.down.sql".
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlmigrate: read migrations: %w", err)
	}

	byVersion := make(map[int]*Migration)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}

		content, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("sqlmigrate: read %s: %w", e.Name(), err)
		}

		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}

		if direction == "up" {
			m.Up = string(content)
		} else {
			m.Down = string(content)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("sqlmigrate: migration %d (%s) has no up file", m.Version, m.Name)
		}
		out = append(out, *m)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	return out, nil
}

func parseMigrationName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")

	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("sqlmigrate: migration %q has no direction suffix", filename)
	}

	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("sqlmigrate: migration %q has an unknown direction %q", filename, direction)
	}

	rest := base[:dot]

	underscore := strings.Index(rest, "_")
	if underscore < 0 {
		return 0, "", "", fmt.Errorf("sqlmigrate: migration %q has no version prefix", filename)
	}

	version, err = strconv.Atoi(rest[:underscore])
	if err != nil {
		return 0, "", "", fmt.Errorf("sqlmigrate: migration %q has a non-numeric version: %w", filename, err)
	}

	return version, rest[underscore+1:], direction, nil
}
