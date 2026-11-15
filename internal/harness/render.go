// Package harness turns harness manifests into what a box needs: the
// launch descriptions and seed files under /etc/devc/harness, the rendered
// baseline (settings and instructions) mounted read-only at the manifest's
// baseline directory, the state mounts of the home strategy, the delivery
// of credentials and granted secrets, and logins.
//
// Security rules: everything rendered comes from the configuration
// directory, the embedded templates and devc itself (README rule 1); the
// box receives it through read-only mounts it cannot change. Secret values
// are written only to the box's tmpfs secrets directory, never to argument
// lists, the environment of podman, or logs (rule 5). Names are validated
// before they become paths (rule 4).
package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
)

// maxConfigFile bounds files read from the configuration directory.
const maxConfigFile = 1 << 20

// Data is what templates see.
type Data struct {
	Harness     string
	HookKind    string
	Realm       string
	Project     string
	ProjectDir  string // /w/<realm>/<project>
	MainDir     string // /w/<realm>/<project>/main
	Home        string // /home/dev
	HarnessHome string // /home/dev/<home.dir>
	Helper      string // /opt/devc/bin/devc
	Notes       string
	Memory      string
	Network     string
	// NetworkSentence describes the network level in one sentence.
	NetworkSentence string
	// Data is set when the project defines data zones.
	Data *DataFacts
	// Instructions is the assembled instruction text (settings templates).
	Instructions string
	// QuestionHookTimeout is the timeout (seconds) of a hook that waits for
	// an answer to a question: input.question_wait plus a minute, so the
	// harness never kills the hook while it waits.
	QuestionHookTimeout int
	// AutoRequests are the request kinds approved without asking; AutoSeal
	// says whether seal is one of them.
	AutoRequests []string
	AutoSeal     bool
	// SSH is set when the project has SSH grants (work order C10b).
	SSH *SSHFacts
}

// SSHFacts describes the hosts a box reaches through the SSH gate.
type SSHFacts struct {
	// Hosts are granted fully, ReadOnly with a list of allowed commands.
	Hosts    []string
	ReadOnly []SSHReadOnly
}

// SSHReadOnly is one host of the read-only profile.
type SSHReadOnly struct {
	Host  string
	Allow []string
}

// DataFacts describes the project's data for the sandbox description.
type DataFacts struct {
	Root   string
	Inputs []string
	Work   []string
	Sealed []string
}

var networkSentences = map[string]string{
	"none": "there is no network access.",
	"allowlist": "only the hosts on this project's allowlist are reachable, through a proxy that the host runs at http://127.0.0.1:3128 " +
		"(HTTP_PROXY and HTTPS_PROXY point to it). The box has no other network: programs that ignore the proxy variables reach nothing, " +
		"there is no DNS, and nothing at all is reachable while the devc daemon on the host is not running. The proxy refuses IP addresses " +
		"and names that resolve to private addresses. If the task needs another host, run `devc request net <name>:<port>` and wait for the user. " +
		"To serve something to the user, listen on 127.0.0.1 in the box and ask the user to run `devc forward <port>`.",
	"internet": "the internet is reachable, but the host, its local network and other private addresses are not. DNS works; IPv6 is off. " +
		"If the task needs a LAN host or a service on the host, run `devc request net <address>:<port>` or `devc request net host:<port>` " +
		"(the host's service then appears at 127.0.0.1:<port> in the box) and wait for the user. " +
		"To serve something to the user, listen on 127.0.0.1 in the box and ask the user to run `devc forward <port>`.",
	"open": "the network is not restricted: the host's local network and services are reachable too.",
}

// NewData collects the template data of a project and harness.
func NewData(r *config.Resolved, m *config.Harness) Data {
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	pdir := paths.InBoxProjectDir(ref)
	d := Data{
		Harness: m.ID, HookKind: m.Hooks.Kind,
		Realm: r.Realm, Project: r.Project,
		ProjectDir: pdir, MainDir: path.Join(pdir, "main"),
		Home: paths.InBoxHome, HarnessHome: path.Join(paths.InBoxHome, m.Home.Dir),
		Helper: paths.InBoxHelper, Notes: paths.InBoxNotes, Memory: paths.InBoxMemory,
		Network: r.Network, NetworkSentence: networkSentences[r.Network],
		QuestionHookTimeout: m.Input.Wait() + 60,
		AutoRequests:        append([]string{}, r.Requests.Auto...),
		AutoSeal:            slices.Contains(r.Requests.Auto, "seal"),
	}
	// The description must match the level in force: a level devc cannot
	// enforce yet never gets a box, and is never described as restricted.
	if !netpolicy.Enforced(r.Network) || d.NetworkSentence == "" {
		d.NetworkSentence = "level " + r.Network + " is not enforced; assume the network is not restricted."
	}
	if len(r.Grants.SSH) > 0 {
		f := &SSHFacts{}
		for _, g := range r.Grants.SSHGrants() {
			if g.ReadOnly {
				f.ReadOnly = append(f.ReadOnly, SSHReadOnly{Host: g.Host, Allow: r.Grants.SSHAllowed(g.Host)})
			} else {
				f.Hosts = append(f.Hosts, g.Host)
			}
		}
		d.SSH = f
	}
	if r.Data.Enabled {
		join := func(ps []string) []string {
			out := make([]string, len(ps))
			for i, p := range ps {
				out[i] = path.Join(pdir, r.Data.Root, p)
			}
			return out
		}
		d.Data = &DataFacts{Root: path.Join(pdir, r.Data.Root), Inputs: join(r.Data.Inputs), Work: join(r.Data.Work), Sealed: join(r.Data.Sealed)}
	}
	return d
}

// tomlString quotes s as a TOML basic string. JSON string escapes are valid
// TOML escapes; DEL, which JSON leaves raw and TOML forbids raw, is escaped
// as well.
func tomlString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.ReplaceAll(strings.TrimSuffix(buf.String(), "\n"), "\x7f", `\u007f`)
}

func funcs(d Data) template.FuncMap {
	return template.FuncMap{
		"json": func(v any) (string, error) {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err != nil {
				return "", err
			}
			return strings.TrimSuffix(buf.String(), "\n"), nil
		},
		"toml": func(s string) string { return tomlString(s) },
		"join": strings.Join,
		// hook is the command a harness hook runs.
		"hook": func(event string) string {
			return d.Helper + " box hook " + d.HookKind + " " + event
		},
	}
}

// templateSource finds a template: <config>/harness/<name> for a manifest
// from the configuration directory, then the embedded templates.
func templateSource(c *config.Config, m *config.Harness, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("invalid template name %q", name)
	}
	if m == nil || m.Source != "builtin" {
		p := filepath.Join(c.Paths.UserHarnessDir(), name)
		b, err := readConfigFile(p)
		if err == nil {
			return string(b), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	b, err := assets.HarnessTemplates.ReadFile("harness/templates/" + name)
	if err != nil {
		return "", fmt.Errorf("template %s not found in %s or among the built-in templates", name, c.Paths.UserHarnessDir())
	}
	return string(b), nil
}

func execTemplate(name, src string, d Data) (string, error) {
	t, err := template.New(name).Funcs(funcs(d)).Option("missingkey=error").Parse(src)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return buf.String(), nil
}

// readConfigFile reads a regular file from the configuration directory.
func readConfigFile(p string) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Size() > maxConfigFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, maxConfigFile)
	}
	return os.ReadFile(p)
}

// Instructions assembles a harness's instruction text: the generated
// sandbox description, then whichever of <config>/baseline/instructions.md,
// <config>/baseline/<id>/instructions.md and
// <config>/realms/<realm>/instructions.md exist.
func Instructions(c *config.Config, r *config.Resolved, m *config.Harness) (string, error) {
	src, err := templateSource(c, nil, "sandbox.md.tmpl")
	if err != nil {
		return "", err
	}
	desc, err := execTemplate("sandbox.md.tmpl", src, NewData(r, m))
	if err != nil {
		return "", err
	}
	parts := []string{strings.TrimSpace(desc)}
	for _, p := range []string{
		c.Paths.BaselineInstructions(),
		filepath.Join(c.Paths.BaselineDir(m.ID), "instructions.md"),
		c.Paths.RealmInstructions(r.Realm),
	} {
		b, err := readConfigFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}

// Settings renders a harness's baseline settings: the user's file
// <config>/baseline/<id>/settings.json or settings.toml (either format for
// either harness), with the required keys of the manifest's template merged
// over it. It returns nil when the manifest has no settings.
func Settings(c *config.Config, r *config.Resolved, m *config.Harness, instructions string) ([]byte, error) {
	s := m.Baseline.Settings
	if s == nil {
		return nil, nil
	}
	user, err := userSettings(c.Paths.BaselineDir(m.ID))
	if err != nil {
		return nil, err
	}
	required := map[string]any{}
	if s.Template != "" {
		src, err := templateSource(c, m, s.Template)
		if err != nil {
			return nil, err
		}
		d := NewData(r, m)
		d.Instructions = instructions
		out, err := execTemplate(s.Template, src, d)
		if err != nil {
			return nil, err
		}
		if required, err = parseSettings(s.Template, s.Format, []byte(out)); err != nil {
			return nil, fmt.Errorf("the template %s did not render valid %s: %w", s.Template, s.Format, err)
		}
	}
	merged := Merge(user, required)
	return marshalSettings(s.Format, merged)
}

func userSettings(dir string) (map[string]any, error) {
	var found map[string]any
	var foundFile string
	for _, f := range []struct{ name, format string }{{"settings.json", "json"}, {"settings.toml", "toml"}} {
		p := filepath.Join(dir, f.name)
		b, err := readConfigFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if foundFile != "" {
			return nil, fmt.Errorf("both %s and %s exist; keep one", foundFile, p)
		}
		m, err := parseSettings(p, f.format, b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		found, foundFile = m, p
	}
	if found == nil {
		found = map[string]any{}
	}
	return found, nil
}

func parseSettings(name, format string, b []byte) (map[string]any, error) {
	var v map[string]any
	switch format {
	case "json":
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if dec.More() {
			return nil, fmt.Errorf("trailing data after the JSON object")
		}
	case "toml":
		if _, err := toml.Decode(string(b), &v); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown settings format %q", format)
	}
	if v == nil {
		v = map[string]any{}
	}
	return normalize(v).(map[string]any), nil
}

// normalize turns decoded values into map[string]any, []any and scalars,
// and JSON numbers into int64 or float64, so both formats merge alike.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case int:
		return int64(x)
	}
	return v
}

// Merge deep-merges required over user: devc's value wins for every key it
// sets, objects merge recursively, and where both sides hold a list (rules,
// hook groups) the lists are concatenated, user entries first, without
// repeating an entry devc's list already holds.
func Merge(user, required map[string]any) map[string]any {
	out := make(map[string]any, len(user)+len(required))
	for k, v := range user {
		out[k] = v
	}
	for k, rv := range required {
		uv, ok := out[k]
		if !ok {
			out[k] = rv
			continue
		}
		switch r := rv.(type) {
		case map[string]any:
			if u, ok := uv.(map[string]any); ok {
				out[k] = Merge(u, r)
				continue
			}
		case []any:
			if u, ok := uv.([]any); ok {
				merged := []any{}
				for _, e := range u {
					if !containsDeep(r, e) {
						merged = append(merged, e)
					}
				}
				out[k] = append(merged, r...)
				continue
			}
		}
		out[k] = rv
	}
	return out
}

func containsDeep(list []any, v any) bool {
	for _, e := range list {
		if reflect.DeepEqual(e, v) {
			return true
		}
	}
	return false
}

func marshalSettings(format string, v map[string]any) ([]byte, error) {
	switch format {
	case "json":
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "toml":
		var buf bytes.Buffer
		buf.WriteString("# Rendered by devc from the baseline; changes here are overwritten.\n")
		if err := toml.NewEncoder(&buf).Encode(dropNulls(v)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unknown settings format %q", format)
}

// dropNulls removes JSON nulls, which TOML cannot express.
func dropNulls(v map[string]any) map[string]any {
	out := map[string]any{}
	for k, e := range v {
		switch x := e.(type) {
		case nil:
		case map[string]any:
			out[k] = dropNulls(x)
		case []any:
			l := []any{}
			for _, y := range x {
				if m, ok := y.(map[string]any); ok {
					l = append(l, dropNulls(m))
				} else if y != nil {
					l = append(l, y)
				}
			}
			out[k] = l
		default:
			out[k] = e
		}
	}
	return out
}

// RenderBaselineFile renders one of a manifest's baseline.files from its
// template alone and checks that it parses in its format.
func RenderBaselineFile(c *config.Config, r *config.Resolved, m *config.Harness, f config.BaselineFile, instructions string) ([]byte, error) {
	src, err := templateSource(c, m, f.Template)
	if err != nil {
		return nil, err
	}
	d := NewData(r, m)
	d.Instructions = instructions
	out, err := execTemplate(f.Template, src, d)
	if err != nil {
		return nil, err
	}
	if _, err := parseSettings(f.Template, f.Format, []byte(out)); err != nil {
		return nil, fmt.Errorf("the template %s did not render valid %s: %w", f.Template, f.Format, err)
	}
	return []byte(out), nil
}

// RenderSeed renders one seed template; JSON seeds are checked and
// re-indented, so a broken template fails at `devc up`.
func RenderSeed(c *config.Config, r *config.Resolved, m *config.Harness, seed config.HarnessSeed) ([]byte, error) {
	src, err := templateSource(c, m, seed.Template)
	if err != nil {
		return nil, err
	}
	out, err := execTemplate(seed.Template, src, NewData(r, m))
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(seed.Path, ".json") {
		var v any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			return nil, fmt.Errorf("seed %s: the template %s did not render valid JSON: %w", seed.Path, seed.Template, err)
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
	return []byte(out), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
