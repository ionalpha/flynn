// Package deploy holds the files an operator installs to run Flynn: container configs, the
// runner systemd unit and the AppArmor profile. It declares no Go API; the tests here
// hold the shipped unit and profile to what the Linux sandbox needs and to nothing more.
package deploy

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

const (
	runnerUnitPath  = "systemd/flynn-runner.service"
	baselineUnit    = "testdata/hardened-baseline.service"
	apparmorProfile = "apparmor/flynn"
)

// directive is one setting line of a unit file.
type directive struct {
	section string
	key     string
	value   string
	line    int
	// commented is true when a comment line sits directly above this directive or above
	// the run of directives it belongs to.
	commented bool
}

func (d directive) String() string { return d.section + " " + d.key + "=" + d.value }

// parseUnit reads a unit file into its directives, in order. Blank lines and comments
// carry no setting; a comment above a run of directives marks the whole run as explained.
func parseUnit(text string) []directive {
	var out []directive
	section := ""
	explained := false
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			explained = false
		case strings.HasPrefix(line, "#"):
			explained = true
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line
			explained = false
		default:
			key, value, _ := strings.Cut(line, "=")
			out = append(out, directive{section: section, key: strings.TrimSpace(key), value: strings.TrimSpace(value), line: i + 1, commented: explained})
		}
	}
	return out
}

// sandboxKeys are the only directives the runner unit may differ from the baseline in.
var sandboxKeys = []string{"RestrictNamespaces", "SystemCallFilter"}

// violations lists everything wrong with a runner unit measured against the plain
// hardened baseline. The shipped unit must return none; the mutation test below shows each
// rule fires.
func violations(runner, baseline string) []string {
	r, b := parseUnit(runner), parseUnit(baseline)
	var v []string
	v = append(v, diffViolations(r, b)...)
	v = append(v, namespaceViolations(r)...)
	v = append(v, syscallViolations(r, b)...)
	v = append(v, privilegeViolations(r)...)
	v = append(v, hardeningViolations(r)...)
	v = append(v, secretViolations(r)...)
	v = append(v, commentViolations(r)...)
	return v
}

// only returns the directives of a that b does not have, matching each at most once.
func only(a, b []directive) []directive {
	left := slices.Clone(b)
	var out []directive
	for _, d := range a {
		i := slices.IndexFunc(left, func(x directive) bool { return x.String() == d.String() })
		if i < 0 {
			out = append(out, d)
			continue
		}
		left = slices.Delete(left, i, i+1)
	}
	return out
}

func diffViolations(runner, baseline []directive) []string {
	var v []string
	for _, side := range []struct {
		name string
		ds   []directive
	}{{"runner unit", only(runner, baseline)}, {"baseline", only(baseline, runner)}} {
		for _, d := range side.ds {
			if !slices.Contains(sandboxKeys, d.key) {
				v = append(v, "diff: "+side.name+" differs in "+d.String()+", which is not a namespace or syscall line")
			}
		}
	}
	return v
}

func values(ds []directive, key string) []string {
	var out []string
	for _, d := range ds {
		if d.key == key {
			out = append(out, d.value)
		}
	}
	return out
}

func namespaceViolations(runner []directive) []string {
	ns := values(runner, "RestrictNamespaces")
	if len(ns) != 1 {
		return []string{"namespaces: want exactly one RestrictNamespaces line"}
	}
	got := strings.Fields(ns[0])
	want := sandbox.RequiredNamespaces()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return []string{"namespaces: RestrictNamespaces=" + ns[0] + " is not exactly the sandbox's " + strings.Join(want, " ")}
	}
	return nil
}

func syscallViolations(runner, baseline []directive) []string {
	var v []string
	filters := values(runner, "SystemCallFilter")
	var added []string
	for _, d := range only(runner, baseline) {
		if d.key == "SystemCallFilter" {
			added = append(added, d.value)
		}
	}
	if !slices.Equal(added, sandbox.RequiredSyscalls()) {
		v = append(v, "syscalls: the unit adds "+strings.Join(added, ",")+" to the baseline filter, want exactly "+strings.Join(sandbox.RequiredSyscalls(), ","))
	}
	// The call is allowed back only if it comes after the exclusion that removes it: in
	// systemd the lines apply in order.
	exclude := slices.IndexFunc(filters, func(f string) bool { return strings.HasPrefix(f, "~") })
	allow := slices.Index(filters, "mount")
	if exclude < 0 || allow < exclude {
		v = append(v, "syscalls: mount must be allowed after the ~ exclusion that removes it")
	}
	if !slices.Equal(values(runner, "SystemCallErrorNumber"), []string{"EPERM"}) {
		v = append(v, "syscalls: SystemCallErrorNumber must be set so a refused call fails instead of killing the process")
	}
	return v
}

func privilegeViolations(runner []directive) []string {
	var v []string
	for _, key := range []string{"CapabilityBoundingSet", "AmbientCapabilities"} {
		got := values(runner, key)
		if len(got) != 1 || got[0] != "" {
			v = append(v, "privilege: "+key+" must be set once and empty, got "+strings.Join(got, "|"))
		}
	}
	for _, d := range runner {
		if strings.Contains(d.value, "CAP_") {
			v = append(v, "privilege: "+d.String()+" names a capability")
		}
		if strings.HasPrefix(d.key, "Exec") && d.value != "" && strings.ContainsRune("+!@:", rune(d.value[0])) {
			v = append(v, "privilege: "+d.String()+" runs with a privileged or special prefix")
		}
	}
	if got := values(runner, "NoNewPrivileges"); !slices.Equal(got, []string{"true"}) {
		v = append(v, "privilege: NoNewPrivileges must be true, got "+strings.Join(got, "|"))
	}
	if got := values(runner, "ProtectSystem"); !slices.Equal(got, []string{"strict"}) {
		v = append(v, "privilege: ProtectSystem must be strict, got "+strings.Join(got, "|"))
	}
	user := values(runner, "User")
	if len(user) != 1 || user[0] == "" || user[0] == "root" || user[0] == "0" {
		v = append(v, "privilege: the unit must run as a named unprivileged user, got "+strings.Join(user, "|"))
	}
	return v
}

// mustStay are the directives the sandbox has no reason to loosen, each with the one value
// it must keep. The check reads the unit alone, so it holds even if the baseline were
// edited to match a loosened unit.
var mustStay = []struct{ key, value string }{
	{"NoNewPrivileges", "true"},
	{"ProtectSystem", "strict"},
	{"ProtectHome", "true"},
	{"PrivateTmp", "true"},
	{"RestrictSUIDSGID", "true"},
	{"RuntimeDirectoryMode", "0700"},
	{"SystemCallErrorNumber", "EPERM"},
}

func hardeningViolations(runner []directive) []string {
	var v []string
	for _, m := range mustStay {
		if got := values(runner, m.key); !slices.Equal(got, []string{m.value}) {
			v = append(v, "hardening: "+m.key+" must be "+m.value+", got "+strings.Join(got, "|"))
		}
	}
	// The filter is an allow-list: its first line names a group to allow, and the lines
	// after it only subtract from it.
	if filters := values(runner, "SystemCallFilter"); len(filters) == 0 || strings.HasPrefix(filters[0], "~") {
		v = append(v, "hardening: SystemCallFilter must start from an allow-list, got "+strings.Join(filters, "|"))
	}
	return v
}

// secretLike matches a value shaped like a credential, so a key pasted into the unit is
// caught wherever it lands.
var secretLike = regexp.MustCompile(`(?i)(sk-[a-z0-9]{8,}|api[_-]?key|token|secret|password|bearer )`)

// secretViolations keeps credentials out of the unit file, which any local account can
// read. A secrets file is allowed, from /etc only: the unit names where it is, and the
// file's own mode keeps it private.
func secretViolations(runner []directive) []string {
	var v []string
	for _, d := range runner {
		switch {
		case d.key == "Environment" || d.key == "SetCredential" || d.key == "SetCredentialEncrypted":
			v = append(v, "secrets: "+d.String()+" puts a value in the unit file, which any local account can read")
		case d.key == "EnvironmentFile" && !strings.HasPrefix(strings.TrimPrefix(d.value, "-"), "/etc/"):
			v = append(v, "secrets: "+d.String()+" reads secrets from outside /etc")
		case d.key != "EnvironmentFile" && secretLike.MatchString(d.value):
			v = append(v, "secrets: "+d.String()+" looks like a credential")
		}
	}
	return v
}

func commentViolations(runner []directive) []string {
	var v []string
	for _, d := range runner {
		if !d.commented {
			v = append(v, "comment: "+d.String()+" has no comment saying why it is there")
		}
	}
	return v
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunnerUnitDiffLimitedToNamespaceAndSyscallLines is the widening check: against a
// unit with the usual hardening and no allowance for the sandbox, the shipped unit differs
// in the namespace and syscall lines and in nothing else.
func TestRunnerUnitDiffLimitedToNamespaceAndSyscallLines(t *testing.T) {
	runner, baseline := readFile(t, runnerUnitPath), readFile(t, baselineUnit)
	for _, v := range violations(runner, baseline) {
		t.Error(v)
	}

	r, b := parseUnit(runner), parseUnit(baseline)
	var gotRunner, gotBase []string
	for _, d := range only(r, b) {
		gotRunner = append(gotRunner, d.key+"="+d.value)
	}
	for _, d := range only(b, r) {
		gotBase = append(gotBase, d.key+"="+d.value)
	}
	if want := []string{"RestrictNamespaces=user net mnt", "SystemCallFilter=mount"}; !slices.Equal(gotRunner, want) {
		t.Errorf("lines only in the runner unit = %q, want %q", gotRunner, want)
	}
	if want := []string{"RestrictNamespaces=true"}; !slices.Equal(gotBase, want) {
		t.Errorf("lines only in the baseline = %q, want %q", gotBase, want)
	}
}

// TestRunnerUnitRulesCatchWidening shows every rule behind the diff check fires: each
// mutation widens the unit in one way, and the check must name it. A check that passed
// the real unit but missed these would prove nothing.
func TestRunnerUnitRulesCatchWidening(t *testing.T) {
	runner, baseline := readFile(t, runnerUnitPath), readFile(t, baselineUnit)
	if got := violations(runner, baseline); len(got) != 0 {
		t.Fatalf("the shipped unit already violates its own rules: %v", got)
	}
	tests := []struct {
		name   string
		from   string
		to     string
		expect string
		edit   func(string) string // used instead of from/to when set
	}{
		{"cap sys admin in the bounding set", "CapabilityBoundingSet=\n", "CapabilityBoundingSet=CAP_SYS_ADMIN\n", "privilege", nil},
		{"ambient capability", "AmbientCapabilities=\n", "AmbientCapabilities=CAP_NET_BIND_SERVICE\n", "privilege", nil},
		{"bounding set removed", "CapabilityBoundingSet=\n", "", "privilege", nil},
		{"no new privileges off", "NoNewPrivileges=true", "NoNewPrivileges=false", "privilege", nil},
		{"no new privileges removed", "NoNewPrivileges=true\n", "", "privilege", nil},
		{"weaker file system protection", "ProtectSystem=strict", "ProtectSystem=full", "privilege", nil},
		{"runs as root", "User=flynn", "User=root", "privilege", nil},
		{"privileged exec prefix", "ExecStart=/usr/local/bin/flynn", "ExecStart=+/usr/local/bin/flynn", "privilege", nil},
		{"another namespace type", "\nRestrictNamespaces=user net mnt\n", "\nRestrictNamespaces=user net mnt pid\n", "namespaces", nil},
		{"namespaces unrestricted", "\nRestrictNamespaces=user net mnt\n", "\nRestrictNamespaces=no\n", "namespaces", nil},
		{"a missing namespace type", "\nRestrictNamespaces=user net mnt\n", "\nRestrictNamespaces=user net\n", "namespaces", nil},
		{"the whole mount group", "SystemCallFilter=mount\n", "SystemCallFilter=@mount\n", "syscalls", nil},
		{"the privileged group", "SystemCallFilter=~@privileged @resources @obsolete", "SystemCallFilter=~@resources @obsolete", "syscalls", nil},
		{"mount allowed before the exclusion", "", "", "syscalls", moveMountBeforeExclusion},
		{"the error number removed", "SystemCallErrorNumber=EPERM\n", "", "syscalls", nil},
		{"home directory exposed", "ProtectHome=true", "ProtectHome=false", "hardening", nil},
		{"shared tmp", "PrivateTmp=true", "PrivateTmp=false", "hardening", nil},
		{"setuid files allowed", "RestrictSUIDSGID=true\n", "", "hardening", nil},
		{"runtime directory open to the group", "RuntimeDirectoryMode=0700", "RuntimeDirectoryMode=0750", "hardening", nil},
		{"the syscall filter becomes a deny-list", "SystemCallFilter=@system-service\n", "", "hardening", nil},
		{"a secret in Environment", "[Install]", "Environment=ANTHROPIC_API_KEY=sk-ant-0123456789abcdef\n\n[Install]", "secrets", nil},
		{"a credential value inline", "UMask=0077", "UMask=0077\nSetCredential=key:sk-ant-0123456789abcdef", "secrets", nil},
		{"secrets read from a temporary file", "EnvironmentFile=-/etc/flynn/flynn.env", "EnvironmentFile=-/tmp/flynn.env", "secrets", nil},
		{"every namespace type allowed", "\nRestrictNamespaces=user net mnt\n", "\nRestrictNamespaces=yes\n", "namespaces", nil},
		{"the user type excluded", "\nRestrictNamespaces=user net mnt\n", "\nRestrictNamespaces=~user\n", "namespaces", nil},
		{"an unrelated hardening line dropped", "ProtectKernelModules=true\n", "", "diff", nil},
		{"an unrelated line added", "[Install]", "BindPaths=/\n\n[Install]", "diff", nil},
		{"a line without a reason", "ProtectKernelModules=true\n", "ProtectKernelModules=true\n\nSomeSetting=1\n", "diff", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(runner, tc.from, tc.to, 1)
			if tc.edit != nil {
				mutated = tc.edit(runner)
			}
			if mutated == runner {
				t.Fatalf("mutation %q did not change the unit; the test is stale", tc.from)
			}
			got := violations(mutated, baseline)
			if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, tc.expect) }) {
				t.Fatalf("a unit with %q passed the %s rule: %v", tc.to, tc.expect, got)
			}
		})
	}

	t.Run("an uncommented line", func(t *testing.T) {
		mutated := strings.Replace(runner, "ProtectKernelModules=true\n", "ProtectKernelModules=true\n\nProtectHostname=true\n", 1)
		if got := violations(mutated, baseline); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "comment") }) {
			t.Fatalf("a line with no reason passed: %v", got)
		}
	})
}

// moveMountBeforeExclusion puts the mount allowance ahead of the line that removes it,
// the order in which systemd would drop it again.
func moveMountBeforeExclusion(unit string) string {
	const mount, exclude = "\nSystemCallFilter=mount\n", "\nSystemCallFilter=~@privileged"
	unit = strings.Replace(unit, mount, "\n", 1)
	return strings.Replace(unit, exclude, mount+exclude[1:], 1)
}

// TestRunnerUnitNamesNoCapability holds the unit to the least it can say about
// privilege on its own, without the baseline: the bounding and ambient sets are empty and
// CAP_SYS_ADMIN appears nowhere, comments included.
func TestRunnerUnitNamesNoCapability(t *testing.T) {
	text := readFile(t, runnerUnitPath)
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "CAP_") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("a directive names a capability: %q", line)
		}
	}
	if strings.Contains(text, "CAP_SYS_ADMIN") {
		// The header comment may warn against it, but only by that exact wording.
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "CAP_SYS_ADMIN") && !strings.Contains(line, "Never add CAP_SYS_ADMIN") {
				t.Errorf("CAP_SYS_ADMIN appears outside the warning against it: %q", line)
			}
		}
	}
}

// TestRunnerUnitStaysASingleService keeps the unit to one file that reads as text with
// the three standard sections, so a stray byte or an include cannot widen it unseen.
func TestRunnerUnitStaysASingleService(t *testing.T) {
	text := readFile(t, runnerUnitPath)
	if strings.Contains(text, "\r") {
		t.Error("the unit has carriage returns; systemd units are LF text")
	}
	for _, header := range []string{"[Unit]", "[Service]", "[Install]"} {
		if strings.Count(text, header+"\n") != 1 {
			t.Errorf("want exactly one %s section", header)
		}
	}
	for _, d := range parseUnit(text) {
		if d.key == "ReadWritePaths" || d.key == "BindPaths" || d.key == "InaccessiblePaths" || d.key == "Environment" {
			t.Errorf("%s changes the file system view or environment; the unit should not need it", d)
		}
	}
}

var profileHeader = regexp.MustCompile(`^profile flynn (/\S+) flags=\(unconfined\) \{$`)

// TestAppArmorProfileGrantsOnlyUserns reads the profile as text and holds it to the one
// permission it exists for: every rule line is on a short list, userns is the only
// permission, the profile stays unconfined (so it narrows nothing and grants nothing but
// userns), and it names the binary the unit runs.
func TestAppArmorProfileGrantsOnlyUserns(t *testing.T) {
	text := readFile(t, apparmorProfile)
	if strings.Contains(text, "\r") {
		t.Error("the profile has carriage returns")
	}
	var rules []string
	binary := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := profileHeader.FindStringSubmatch(line); m != nil {
			binary = m[1]
			rules = append(rules, "header")
			continue
		}
		rules = append(rules, line)
	}
	want := []string{"abi <abi/4.0>,", "include <tunables/global>", "header", "userns,", "include if exists <local/flynn>", "}"}
	if !slices.Equal(rules, want) {
		t.Fatalf("profile rules = %q, want exactly %q", rules, want)
	}
	if strings.Count(text, "{") != 1 || strings.Count(text, "}") != 1 {
		t.Error("braces do not balance to a single profile block")
	}
	for _, forbidden := range []string{"capability", "network", "mount", "ptrace", "signal", "dbus", "rw,", "x,"} {
		for _, line := range rules {
			if strings.Contains(line, forbidden) {
				t.Errorf("profile line %q grants or mentions %q", line, forbidden)
			}
		}
	}
	execStart := values(parseUnit(readFile(t, runnerUnitPath)), "ExecStart")
	if len(execStart) != 1 || !strings.HasPrefix(execStart[0], binary+" ") {
		t.Errorf("profile attaches to %q but the unit runs %q", binary, execStart)
	}
}

// TestRunnerUnitKeepsHardening holds each directive the sandbox leaves alone to its value,
// one case per directive, so a failure names the one that moved.
func TestRunnerUnitKeepsHardening(t *testing.T) {
	unit := parseUnit(readFile(t, runnerUnitPath))
	for _, m := range mustStay {
		t.Run(m.key, func(t *testing.T) {
			if got := values(unit, m.key); !slices.Equal(got, []string{m.value}) {
				t.Fatalf("%s = %q, want %q", m.key, got, m.value)
			}
		})
	}
	for _, key := range []string{"CapabilityBoundingSet", "AmbientCapabilities"} {
		t.Run(key, func(t *testing.T) {
			if got := values(unit, key); !slices.Equal(got, []string{""}) {
				t.Fatalf("%s = %q, want it set once and empty", key, got)
			}
		})
	}
	t.Run("User", func(t *testing.T) {
		got := values(unit, "User")
		if len(got) != 1 || got[0] == "" || got[0] == "root" || got[0] == "0" {
			t.Fatalf("User = %q, want a named unprivileged account", got)
		}
	})
	t.Run("SystemCallFilter", func(t *testing.T) {
		filters := values(unit, "SystemCallFilter")
		if len(filters) == 0 || strings.HasPrefix(filters[0], "~") {
			t.Fatalf("SystemCallFilter = %q, want it to start from an allow-list", filters)
		}
	})
}

// TestRunnerUnitNamespaceSet holds the namespace line to the types the sandbox creates. It
// is never a boolean (which allows every type or none, depending on the systemd version)
// and never a deny-list such as "~user". The base filter group excludes mount, so the unit
// must name it.
func TestRunnerUnitNamespaceSet(t *testing.T) {
	unit := parseUnit(readFile(t, runnerUnitPath))
	ns := values(unit, "RestrictNamespaces")
	if len(ns) != 1 {
		t.Fatalf("want one RestrictNamespaces line, got %q", ns)
	}
	if slices.Contains([]string{"yes", "true", "1", "on", "no", "false", "0", "off"}, ns[0]) {
		t.Errorf("RestrictNamespaces=%s is a boolean, not the list of types the sandbox needs", ns[0])
	}
	for _, f := range strings.Fields(ns[0]) {
		if strings.HasPrefix(f, "~") {
			t.Errorf("RestrictNamespaces has the deny-list entry %q; the unit must list what it allows", f)
		}
	}
	got, want := strings.Fields(ns[0]), sandbox.RequiredNamespaces()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("RestrictNamespaces = %q, want exactly %q", got, want)
	}
	filters := values(unit, "SystemCallFilter")
	if filters[0] == "@system-service" && !slices.Contains(filters, "mount") && !slices.Contains(filters, "@mount") {
		t.Error("the base is @system-service, which excludes mount; the unit must allow it by name")
	}
}

// TestRunnerUnitRuntimeDirMode keeps the runtime directory owner-only: the control socket
// lives there, and a looser mode would let another account on the host reach it.
func TestRunnerUnitRuntimeDirMode(t *testing.T) {
	unit := parseUnit(readFile(t, runnerUnitPath))
	if got := values(unit, "RuntimeDirectory"); !slices.Equal(got, []string{"flynn"}) {
		t.Fatalf("RuntimeDirectory = %q, want flynn", got)
	}
	if got := values(unit, "RuntimeDirectoryMode"); !slices.Equal(got, []string{"0700"}) {
		t.Fatalf("RuntimeDirectoryMode = %q, want 0700", got)
	}
}

// TestRunnerUnitNoSecretsInEnvironment keeps credentials out of the unit file: no
// Environment= or inline credential, no value shaped like a key, and a secrets file only
// from /etc.
func TestRunnerUnitNoSecretsInEnvironment(t *testing.T) {
	text := readFile(t, runnerUnitPath)
	for _, v := range secretViolations(parseUnit(text)) {
		t.Error(v)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "sk-") || strings.Contains(line, "Bearer ") {
			t.Errorf("the unit contains a key-shaped string: %q", line)
		}
	}
}
