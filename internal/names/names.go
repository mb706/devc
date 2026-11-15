// Package names validates every name that can end up in a path, a container
// name, a git ref or a label, and derives box names, box IDs and session IDs.
//
// It carries security rule 4 of docs/dev/README.md: realm, project, env,
// worktree, harness and session names must be validated here before use.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Kinds of names, used in error messages.
const (
	KindRealm    = "realm"
	KindProject  = "project"
	KindEnv      = "env"
	KindWorktree = "worktree"
	KindHarness  = "harness"
	KindSecret   = "secret"
	KindRemote   = "remote"
	KindGroup    = "memory group"
)

// MaxLen is the longest valid name.
const MaxLen = 63

// MaxRealmLen is the longest valid realm name.
const MaxRealmLen = 32

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	realmRE = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
)

// Valid reports whether s matches ^[a-z0-9][a-z0-9._-]{0,62}$.
func Valid(s string) bool {
	return nameRE.MatchString(s)
}

// ValidRealm reports whether s matches ^[a-z0-9]{1,32}$. Realm names are
// stricter than other names: box names are "devc-<realm>-<project>", and a
// realm containing "-" would make two different projects share a name.
func ValidRealm(s string) bool {
	return realmRE.MatchString(s)
}

// ValidKind applies the rule for kind: the realm rule for KindRealm, the
// general name rule otherwise.
func ValidKind(kind, s string) bool {
	if kind == KindRealm {
		return ValidRealm(s)
	}
	return Valid(s)
}

// Check returns nil if s is a valid name of the given kind, otherwise an
// error naming the kind and the first offending part of s.
func Check(kind, s string) error {
	if kind == KindRealm {
		return checkRealm(s)
	}
	if Valid(s) {
		return nil
	}
	var reason string
	switch {
	case s == "":
		reason = "it is empty"
	case len(s) > MaxLen:
		reason = fmt.Sprintf("it is %d characters long, the limit is %d", len(s), MaxLen)
	case !isLowerAlnum(s[0]):
		reason = fmt.Sprintf("it must start with a lower-case letter or a digit, not %q", firstRune(s))
	default:
		for i, r := range s {
			if !(r < 0x80 && (isLowerAlnum(byte(r)) || r == '.' || r == '_' || r == '-')) {
				reason = fmt.Sprintf("character %q at position %d is not allowed", r, i+1)
				break
			}
		}
	}
	return fmt.Errorf("invalid %s name %q: %s (allowed: a-z, 0-9, '.', '_', '-'; first character a-z or 0-9; at most %d characters)",
		kind, s, reason, MaxLen)
}

func checkRealm(s string) error {
	if ValidRealm(s) {
		return nil
	}
	var reason string
	switch {
	case s == "":
		reason = "it is empty"
	case len(s) > MaxRealmLen:
		reason = fmt.Sprintf("it is %d characters long, the limit is %d", len(s), MaxRealmLen)
	default:
		for i, r := range s {
			if !(r < 0x80 && isLowerAlnum(byte(r))) {
				reason = fmt.Sprintf("character %q at position %d is not allowed", r, i+1)
				break
			}
		}
	}
	return fmt.Errorf("invalid realm name %q: %s (allowed: a-z and 0-9 only, at most %d characters)", s, reason, MaxRealmLen)
}

func isLowerAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

// ProjectRef identifies a project within a realm.
type ProjectRef struct {
	Realm   string `json:"realm" toml:"realm"`
	Project string `json:"project" toml:"project"`
}

// Parse parses "realm/project" and validates both parts.
func Parse(s string) (ProjectRef, error) {
	realm, project, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(project, "/") {
		return ProjectRef{}, fmt.Errorf("invalid project reference %q: expected <realm>/<project>", s)
	}
	if err := Check(KindRealm, realm); err != nil {
		return ProjectRef{}, err
	}
	if err := Check(KindProject, project); err != nil {
		return ProjectRef{}, err
	}
	return ProjectRef{Realm: realm, Project: project}, nil
}

// Validate checks both parts of an already constructed reference.
func (r ProjectRef) Validate() error {
	if err := Check(KindRealm, r.Realm); err != nil {
		return err
	}
	return Check(KindProject, r.Project)
}

func (r ProjectRef) String() string {
	return r.Realm + "/" + r.Project
}

// BoxName is the container name of the project's box (plan 3.1).
func BoxName(r ProjectRef) string {
	return "devc-" + r.Realm + "-" + r.Project
}

// BoxIDLen is the number of hex characters in a box ID.
const BoxIDLen = 12

// BoxID is the short identifier used for runtime paths: the first 12 hex
// characters of SHA-256 of "<realm>/<project>" (plan 3.1).
func BoxID(r ProjectRef) string {
	sum := sha256.Sum256([]byte(r.String()))
	return hex.EncodeToString(sum[:])[:BoxIDLen]
}

var boxIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidBoxID reports whether s has the form produced by BoxID.
func ValidBoxID(s string) bool {
	return boxIDRE.MatchString(s)
}
