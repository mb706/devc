package netpolicy

// Presets are the built-in allowlist presets (work order C13 section 1),
// all in this one file. Each is a short explicit list of the names a tool
// needs, never a wildcard over a hosting provider: a name that serves
// anyone's content (a CDN, an object store) would open the allowlist to
// everything hosted there. Entries use the syntax of
// network.allowlist.domains; without a port an entry allows 80 and 443.
var Presets = map[string][]string{
	// Claude Code: the API, and the login and console pages.
	"anthropic": {"api.anthropic.com", "claude.ai", "console.anthropic.com"},
	// Codex: the API, the login service and ChatGPT's backend.
	"openai": {"api.openai.com", "auth.openai.com", "chatgpt.com"},
	// git over HTTPS, the API, archives, raw files and release downloads.
	"github": {"github.com", "api.github.com", "codeload.github.com", "raw.githubusercontent.com",
		"objects.githubusercontent.com", "release-assets.githubusercontent.com"},
	// pip and uv: the index and the file host.
	"pypi": {"pypi.org", "files.pythonhosted.org"},
	// npm, pnpm, yarn: the registry.
	"npm": {"registry.npmjs.org"},
	// R packages from CRAN.
	"cran": {"cloud.r-project.org", "cran.r-project.org"},
	// apt on Debian images.
	"debian": {"deb.debian.org", "security.debian.org"},
}

// PresetNames lists the presets in a stable order (for messages).
func PresetNames() []string {
	return []string{"anthropic", "cran", "debian", "github", "npm", "openai", "pypi"}
}
