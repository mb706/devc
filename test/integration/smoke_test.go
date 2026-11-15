//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mb706/devc/test/integration/itest"
)

func TestSmoke(t *testing.T) {
	e := itest.New(t)

	res := e.MustRun("version")
	if !strings.HasPrefix(res.Stdout, "devc ") {
		t.Errorf("version: %q", res.Stdout)
	}

	res = e.MustRun("config", "check")
	if !strings.Contains(res.Stdout, "configuration OK") {
		t.Errorf("config check: %q", res.Stdout)
	}

	res = e.Run("doctor", "--json")
	if res.ExitCode != 0 && res.ExitCode != 1 {
		t.Fatalf("doctor: exit %d, stderr %s", res.ExitCode, res.Stderr)
	}
	var checks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &checks); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, res.Stdout)
	}
	byID := map[string]string{}
	for _, c := range checks {
		byID[c.ID] = c.Status
		t.Logf("doctor %-16s %-4s %s", c.ID, c.Status, c.Detail)
	}
	if len(checks) != 14 {
		t.Errorf("doctor reported %d checks, want 14", len(checks))
	}
	// Integration tests need a working rootless engine.
	for _, id := range []string{"engine", "rootless", "runtime-dir"} {
		if byID[id] != "ok" {
			t.Errorf("doctor check %s = %q, want ok", id, byID[id])
		}
	}

	res = e.MustRun("project", "ls", "--json")
	var projects []map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &projects); err != nil || len(projects) != 3 {
		t.Errorf("project ls: %v %s", err, res.Stdout)
	}
	if hd, _ := projects[0]["host_dir"].(string); !strings.HasPrefix(hd, e.ProjectsRoot) {
		t.Errorf("host_dir %q is not under the temporary projects root", hd)
	}

	res = e.MustRun("config", "show", "--json")
	var g map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &g); err != nil || g["state_dir"] != e.StateDir || g["runtime_dir"] != e.RuntimeDir {
		t.Errorf("config show: %v %s", err, res.Stdout)
	}
}

func TestConfigCheckRejectsBrokenConfig(t *testing.T) {
	e := itest.New(t)
	e.WriteConfig("realms/"+itest.RealmA+"/projects/hello/project.toml", "harnesses = [\"claude\"]\n[[mounts]]\nproject = \""+itest.RealmB+"/tools\"\n")
	e.WriteConfig("realms/"+itest.RealmB+"/realm.toml", "[network]\ndefault = \"lan\"\n")
	res := e.Run("config", "check")
	if res.ExitCode != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.ExitCode, res.Stdout)
	}
	for _, want := range []string{
		"realms/" + itest.RealmA + "/projects/hello/project.toml: mounts[0].project:",
		"realms/" + itest.RealmB + "/realm.toml: network.default: \"lan\" is not one of",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("missing %q in:\n%s", want, res.Stdout)
		}
	}
}
