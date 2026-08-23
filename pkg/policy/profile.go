package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Profile fingerprints what decides a subject's requests for one repository,
// ref and action.
//
// **Two subjects with the same profile receive the same decision for every
// path.** That is the whole contract, and it is what makes a filtered
// projection shareable: five hundred developers pulling after a release fall
// into a handful of profiles, and the work can be done once per profile instead
// of once per person.
//
// # Why it is sound
//
// Read Evaluate first. For a fixed repository, ref and action, a decision
// depends on exactly three subject-dependent things:
//
//  1. whether the repository exists — not subject-dependent at all;
//  2. whether the user is disabled;
//  3. which rules pass HasAction, MatchesSubject and MatchesRef.
//
// Everything after that is a function of the path alone: MatchesPath, the
// deny-wins fold, and the specificity tie-break all read the rule and the path,
// never the subject. So two subjects that agree on (2) and (3) agree on every
// path, whatever the path is.
//
// The profile therefore hashes the policy version, the repository, the ref, the
// action, the disabled flag, and the positions of the rules that survive the
// three subject-dependent predicates.
//
// # Why positions rather than rule ids
//
// A position is unique within a compiled policy by construction. A rule id is
// author-supplied or derived, and while the loader rejects duplicates today,
// the profile's correctness would then rest on a validation rule rather than on
// arithmetic. The version pins which compilation the positions refer to, so
// profiles from two bundles can never collide.
//
// # What this is not
//
// It is a sufficient condition, not a necessary one. Two subjects whose rule
// sets differ may still decide identically on every path in practice; they get
// different profiles and the work is done twice. That is the correct direction
// to be wrong in — the other one leaks one person's files to another.
func (p *Policy) Profile(repo RepoID, ref string, action Action, subject Subject) string {
	h := sha256.New()

	// Length-prefixed, so that a repository named "a" with ref "bc" cannot
	// produce the same bytes as one named "ab" with ref "c".
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte(":"))
		h.Write([]byte(s))
	}

	write("nit-profile-v1")
	write(p.version)
	write(string(repo))
	write(ref)
	write(string(action))

	if u, ok := p.users[subject.UserID]; ok && u.Disabled {
		// A disabled user is denied everything, so every disabled user shares
		// one profile — correctly, since they all receive nothing.
		write("disabled")

		return hex.EncodeToString(h.Sum(nil))
	}

	write("enabled")

	// An unknown repository denies everything, and p.rules has no entry for
	// one, so the loop below writes nothing and every subject shares the
	// profile. Also correct, and for the same reason.
	for i, r := range p.rules[repo] {
		if !r.HasAction(action) {
			continue
		}
		if !r.MatchesSubject(subject) {
			continue
		}
		if !r.MatchesRef(ref) {
			continue
		}

		write(strconv.Itoa(i))
	}

	return hex.EncodeToString(h.Sum(nil))
}
