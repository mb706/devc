package github

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Permission levels in increasing order.
var levels = []string{"read", "write", "admin"}

// repoPermissions are the repository permissions an installation token may
// be restricted to, with the levels each takes. workflows is absent on
// purpose (spec 10.1): a token that may change workflows can run code with
// the repository's secrets and escape any branch rule.
var repoPermissions = map[string][]string{
	"actions":                {"read", "write"},
	"administration":         {"read", "write"},
	"attestations":           {"read", "write"},
	"checks":                 {"read", "write"},
	"codespaces":             {"read", "write"},
	"contents":               {"read", "write"},
	"dependabot_secrets":     {"read", "write"},
	"deployments":            {"read", "write"},
	"discussions":            {"read", "write"},
	"environments":           {"read", "write"},
	"issues":                 {"read", "write"},
	"merge_queues":           {"read", "write"},
	"metadata":               {"read"},
	"packages":               {"read", "write"},
	"pages":                  {"read", "write"},
	"pull_requests":          {"read", "write"},
	"repository_hooks":       {"read", "write"},
	"repository_projects":    {"read", "write", "admin"},
	"secret_scanning_alerts": {"read", "write"},
	"secrets":                {"read", "write"},
	"security_events":        {"read", "write"},
	"single_file":            {"read", "write"},
	"statuses":               {"read", "write"},
	"vulnerability_alerts":   {"read", "write"},
}

// DefaultPermissions are used without a [grants.github_token] table.
func DefaultPermissions() map[string]string {
	return map[string]string{"contents": "read", "metadata": "read"}
}

// CheckPermission validates one entry of [grants.github_token]
// permissions.
func CheckPermission(name, level string) error {
	if name == "workflows" {
		return fmt.Errorf("the workflows permission is never granted: a token that can change workflows runs code with the repository's secrets")
	}
	allowed, ok := repoPermissions[name]
	if !ok {
		return fmt.Errorf("%q is not a repository permission devc knows (one of %s)", name, strings.Join(PermissionNames(), ", "))
	}
	if !slices.Contains(allowed, level) {
		return fmt.Errorf("%s = %q: use %s", name, level, strings.Join(allowed, " or "))
	}
	return nil
}

// CheckPermissions validates a whole permissions table.
func CheckPermissions(perms map[string]string) error {
	for _, k := range sortedKeys(perms) {
		if err := CheckPermission(k, perms[k]); err != nil {
			return err
		}
	}
	return nil
}

// PermissionNames lists the known permissions, sorted.
func PermissionNames() []string { return sortedKeys(repoPermissions) }

// IntersectPermissions keeps the permissions both tables grant, each at the
// lower of the two levels (the start record and the configuration now).
func IntersectPermissions(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, la := range a {
		lb, ok := b[k]
		if !ok {
			continue
		}
		ia, ib := slices.Index(levels, la), slices.Index(levels, lb)
		if ia < 0 || ib < 0 {
			continue
		}
		out[k] = levels[min(ia, ib)]
	}
	return out
}

// permissionsKey is a canonical form of a permissions table (cache keys).
func permissionsKey(perms map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(perms) {
		b.WriteString(k + "=" + perms[k] + ",")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
