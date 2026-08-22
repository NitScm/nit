package buildinfo_test

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/buildinfo"
)

// A binary that cannot identify itself turns every bug report into a guess, so
// the one property that matters is that Get always answers something usable —
// including in a plain `go test` build, where no ldflags were set and there may
// be no VCS stamp either.
func TestGetAlwaysIdentifiesTheBuild(t *testing.T) {
	info := buildinfo.Get()

	if info.Version == "" {
		t.Error("Version is empty; a build with no stamp must still say 'dev'")
	}
	if info.Commit == "" {
		t.Error("Commit is empty; a build with no stamp must still say 'unknown'")
	}
	if info.Go == "" || info.Platform == "" {
		t.Errorf("Go = %q, Platform = %q; both come from the runtime and cannot be empty",
			info.Go, info.Platform)
	}
	if !strings.Contains(info.Platform, "/") {
		t.Errorf("Platform = %q, want os/arch", info.Platform)
	}
}

func TestStringIsOneLineAndCarriesTheIdentity(t *testing.T) {
	info := buildinfo.Get()
	line := info.String()

	if strings.Contains(line, "\n") {
		t.Errorf("String() = %q; -version prints one line", line)
	}
	for _, want := range []string{info.Version, info.Commit, info.Go, info.Platform} {
		if !strings.Contains(line, want) {
			t.Errorf("String() = %q, missing %q", line, want)
		}
	}
}
