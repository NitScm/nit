package mysql_test

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/store/mysql"
	"github.com/NitScm/nit/internal/store/sqlmigrate"
	"github.com/NitScm/nit/migrations"
)

// The splitter is what stands between a migration file and the server, which
// takes one statement per round trip. A semicolon it misreads either merges two
// statements into a syntax error or cuts one in half — and the second is worse,
// because half a CREATE TABLE can succeed.
func TestSplitStatements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "two statements",
			input: "SELECT 1; SELECT 2;",
			want:  []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:  "trailing statement without a semicolon",
			input: "SELECT 1;\nSELECT 2",
			want:  []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:  "empty statements are dropped",
			input: ";;\nSELECT 1;;\n",
			want:  []string{"SELECT 1"},
		},
		{
			name:  "a semicolon inside a string ends nothing",
			input: "INSERT INTO t VALUES ('a;b'); SELECT 2",
			want:  []string{"INSERT INTO t VALUES ('a;b')", "SELECT 2"},
		},
		{
			name:  "a semicolon inside a backquoted identifier ends nothing",
			input: "SELECT `we;ird` FROM t; SELECT 2",
			want:  []string{"SELECT `we;ird` FROM t", "SELECT 2"},
		},
		{
			name:  "an escaped quote does not close the string",
			input: `SELECT 'it\'s; fine'; SELECT 2`,
			want:  []string{`SELECT 'it\'s; fine'`, "SELECT 2"},
		},
		{
			name:  "a backslash in a backquoted identifier is literal",
			input: "SELECT `a\\` FROM t; SELECT 2",
			want:  []string{"SELECT `a\\` FROM t", "SELECT 2"},
		},
		{
			name:  "a semicolon inside a line comment ends nothing",
			input: "-- a; comment\nSELECT 1; SELECT 2",
			want:  []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:  "a hash comment is a comment too",
			input: "# a; comment\nSELECT 1",
			want:  []string{"SELECT 1"},
		},
		{
			name:  "a semicolon inside a block comment ends nothing",
			input: "/* a; comment */ SELECT 1; SELECT 2",
			want:  []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:  "a double minus without whitespace is an operator",
			input: "SELECT 1--2",
			want:  []string{"SELECT 1--2"},
		},
		{
			name:  "a trigger body with SIGNAL survives whole",
			input: "CREATE TRIGGER t BEFORE UPDATE ON x FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'no; really'; SELECT 1",
			want: []string{
				"CREATE TRIGGER t BEFORE UPDATE ON x FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'no; really'",
				"SELECT 1",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mysql.SplitStatements(tc.input)
			if err != nil {
				t.Fatalf("SplitStatements: %v", err)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statement %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitStatementsRefusesAnUnterminatedLiteral(t *testing.T) {
	for _, input := range []string{
		"SELECT 'unterminated",
		"SELECT `unterminated",
		"SELECT 1 /* unterminated",
	} {
		if _, err := mysql.SplitStatements(input); err == nil {
			t.Errorf("SplitStatements(%q) succeeded; an unterminated literal must be refused", input)
		}
	}
}

// Every embedded migration has to survive the splitter, and a statement count
// of zero would mean a file that silently does nothing.
func TestEmbeddedMigrationsSplit(t *testing.T) {
	loaded, err := sqlmigrate.Load(migrations.MySQL)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no MySQL migrations are embedded")
	}

	for _, m := range loaded {
		statements, err := mysql.SplitStatements(m.Up)
		if err != nil {
			t.Fatalf("%04d_%s: %v", m.Version, m.Name, err)
		}
		if len(statements) == 0 {
			t.Errorf("%04d_%s: split into no statements", m.Version, m.Name)
		}

		for _, statement := range statements {
			if strings.Contains(statement, "DELIMITER") {
				t.Errorf("%04d_%s: DELIMITER is a mysql(1) directive the server never sees", m.Version, m.Name)
			}
		}
	}
}

// The two dialects must describe the same schema at the same version numbers.
// A migration added to one and forgotten in the other is how a deployment on
// the second backend quietly runs an older schema.
func TestBothDialectsCarryTheSameVersions(t *testing.T) {
	pg, err := sqlmigrate.Load(migrations.Postgres)
	if err != nil {
		t.Fatalf("load postgres: %v", err)
	}

	my, err := sqlmigrate.Load(migrations.MySQL)
	if err != nil {
		t.Fatalf("load mysql: %v", err)
	}

	if len(pg) != len(my) {
		t.Fatalf("%d PostgreSQL migrations and %d MySQL ones", len(pg), len(my))
	}

	for i := range pg {
		if pg[i].Version != my[i].Version || pg[i].Name != my[i].Name {
			t.Errorf("version %d: postgres has %04d_%s, mysql has %04d_%s",
				i, pg[i].Version, pg[i].Name, my[i].Version, my[i].Name)
		}
	}
}
