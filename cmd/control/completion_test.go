package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
)

var completionShells = []string{"bash", "zsh", "fish", "powershell"}

// Role help exactly as commandHelp assembles it.
var completionRoleHelp = map[string]string{
	"common":    usage,
	"user":      usage + fleetUsage,
	"superuser": usage + fleetUsage + adminUsage,
}

func completionNodeAt(t *testing.T, s completionSpec, path string) *completionNode {
	t.Helper()
	var found *completionNode
	s.root.walk(func(n *completionNode) {
		if n.path == path {
			found = n
		}
	})
	if found == nil {
		t.Fatalf("completion has no %q", path)
	}
	return found
}

func TestCompletionParsesHelpCommands(t *testing.T) {
	s := parseCompletionHelp(completionRoleHelp["superuser"])
	subs := map[string][]string{
		"task":             {"start", "get", "wait", "cancel", "logs", "list", "reconcile"},
		"tunnel":           {"start", "list", "dispose"},
		"upload":           {"start", "resume", "status", "abort"},
		"secrets":          {"set", "list", "delete"},
		"artifact":         {"list", "export", "deliver", "get"},
		"service":          {"start", "stop", "status", "uninstall", "firewall"},
		"browser":          {"run", "view"},
		"clipboard":        {"paste"},
		"completion":       {"bash", "zsh", "fish", "powershell"},
		"machines":         {"add", "proxies", "invites", "revoke", "enable", "disable", "rename", "unregister"},
		"machines proxies": {"release", "inspect", "enable", "disable"},
		"machines invites": {"clear"},
		"keys":             {"create", "list", "rename", "revoke"},
		"audit":            {"list", "clear"},
		"users":            {"create", "list", "revoke"},
		"update":           {"authorize", "push", "status", "check"},
		"console":          {"password"},
	}
	for path, want := range subs {
		if got := completionNodeAt(t, s, path).childNames(); !slices.Equal(got, want) {
			t.Errorf("%s subcommands = %v, want %v", path, got, want)
		}
	}
	flags := map[string][]string{
		"":             {"--config", "--api", "--token", "--debug"},
		"task start":   {"--wait", "--spec"},
		"task logs":    {"--follow", "--offset"},
		"tunnel":       {"--reverse", "--listen"},
		"tunnel start": {"--listen", "--reverse", "--id", "--ttl"},
		"runbooks":     {"--node", "--local", "--source", "--file"},
		"machines add": {"--platform", "--service", "--linger", "--force", "--via", "--proxy-url"},
		"machines":     {"--compact", "--node"},
		"update":       {"--version"},
		"upgrade":      {"--version"},
		"browser view": {"--port", "--listen"},
	}
	for path, want := range flags {
		got := completionNodeAt(t, s, path).flags
		for _, flag := range want {
			if !slices.Contains(got, flag) {
				t.Errorf("%q flags %v lack %s", path, got, flag)
			}
		}
	}
	if !slices.Equal(s.valueGlobal, []string{"--config", "--api", "--token"}) {
		t.Fatalf("value-taking global flags = %v", s.valueGlobal)
	}
}

// Every command, subcommand and flag of each role reaches every script.
func TestCompletionScriptsCoverVisibleCommands(t *testing.T) {
	for role, help := range completionRoleHelp {
		s := parseCompletionHelp(help)
		for _, shell := range completionShells {
			script, err := completionScript(shell, help)
			if err != nil {
				t.Fatal(err)
			}
			s.root.walk(func(n *completionNode) {
				if n.path != "" && !strings.Contains(script, "'"+n.path+"'") {
					t.Errorf("%s %s script lacks %q", role, shell, n.path)
				}
				for _, flag := range n.flags {
					if !strings.Contains(script, flag) {
						t.Errorf("%s %s script lacks %s", role, shell, flag)
					}
				}
			})
		}
	}
}

func TestCompletionHidesCommandsHelpHides(t *testing.T) {
	fleet := regexp.MustCompile(`\b(users|keys|audit|console|invites|proxies|unregister|authorize|push|backup)\b`)
	admin := regexp.MustCompile(`\b(users|authorize|push|backup)\b`)
	for _, shell := range completionShells {
		for role, hidden := range map[string]*regexp.Regexp{"common": fleet, "user": admin} {
			script, err := completionScript(shell, completionRoleHelp[role])
			if err != nil {
				t.Fatal(err)
			}
			if m := hidden.FindString(script); m != "" {
				t.Errorf("%s %s completion reveals %q", role, shell, m)
			}
		}
	}
	common := parseCompletionHelp(usage)
	if len(completionNodeAt(t, common, "machines").children) != 0 || len(completionNodeAt(t, common, "update").children) != 0 {
		t.Fatal("common completion offers fleet or administrative subcommands")
	}
	user := parseCompletionHelp(usage + fleetUsage)
	if len(completionNodeAt(t, user, "update").children) != 0 || completionNodeAt(t, user, "machines add") == nil {
		t.Fatal("user completion has the wrong scope")
	}
}

func TestCompletionCLI(t *testing.T) {
	forbidden := func() string { t.Fatal("default completion consulted the login"); return "" }
	var out bytes.Buffer
	if err := completionCLI(&out, []string{"bash"}, forbidden); err != nil || !strings.Contains(out.String(), "complete -o default -F _control control") {
		t.Fatalf("bash completion: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "'machines add'") {
		t.Fatal("default completion exceeded the least-privileged help")
	}
	for _, args := range [][]string{nil, {"tcsh"}, {"bash", "zsh"}, {"--login"}, {"--bogus", "bash"}} {
		out.Reset()
		err := completionCLI(&out, args, forbidden)
		if err == nil || out.Len() != 0 {
			t.Fatalf("completion %q: err %v, output %q", args, err, out.String())
		}
		if args != nil && args[0] == "tcsh" && !strings.Contains(err.Error(), completionUsageText) {
			t.Fatalf("unknown shell error lacks usage: %v", err)
		}
	}
	out.Reset()
	if err := completionCLI(&out, []string{"--help"}, forbidden); err != nil || !strings.Contains(out.String(), completionUsageText) {
		t.Fatalf("completion --help: %v %q", err, out.String())
	}
	for _, args := range [][]string{{"--login", "zsh"}, {"zsh", "--login"}} {
		calls := 0
		out.Reset()
		err := completionCLI(&out, args, func() string { calls++; return usage + fleetUsage })
		if err != nil || calls != 1 || !strings.Contains(out.String(), "'machines add'") {
			t.Fatalf("completion %q: err %v, calls %d", args, err, calls)
		}
	}
	t.Setenv("CONTROL_HOME", t.TempDir())
	if err := run(t.Context(), []string{"completion", "tcsh"}); err == nil || !strings.Contains(err.Error(), completionUsageText) {
		t.Fatalf("run completion tcsh: %v", err)
	}
}

// --login applies exactly the role decision of help.
func TestCompletionLoginFollowsHelpRole(t *testing.T) {
	const common = "common-key-for-completion-tests"
	const super = "superuser-key-for-completion-tests-123456789"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	httpServer := httptest.NewServer(g.Handler())
	defer httpServer.Close()
	server := httpServer.URL
	ctx := context.Background()
	var account struct{ Token string }
	if err = (client.Admin{URL: server, Key: super}).JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": "alice"}, &account); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][2]bool{"": {false, false}, common: {false, false}, account.Token: {true, false}, super: {true, true}} {
		var out bytes.Buffer
		help := func() string { return commandHelp(ctx, client.Admin{URL: server, Key: key}) }
		if err := completionCLI(&out, []string{"bash", "--login"}, help); err != nil {
			t.Fatal(err)
		}
		script := out.String()
		if strings.Contains(script, "'machines add'") != want[0] || strings.Contains(script, "'users create'") != want[1] {
			t.Errorf("key %q: fleet %v admin %v, want %v", key, strings.Contains(script, "'machines add'"), strings.Contains(script, "'users create'"), want)
		}
	}
}

func runFuncDecl(t *testing.T) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" && fn.Recv == nil {
			return fn
		}
	}
	t.Fatal("run not found in main.go")
	return nil
}

func isArgsZero(e ast.Expr) bool {
	index, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	name, ok := index.X.(*ast.Ident)
	lit, isLit := index.Index.(*ast.BasicLit)
	return ok && isLit && name.Name == "args" && lit.Value == "0"
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// The completed top-level commands must equal the commands run dispatches,
// apart from the declared internal ones. A new dispatch case therefore needs
// a help line (and so completion) or an explicit internal entry.
func TestCompletionMatchesDispatch(t *testing.T) {
	dispatched := map[string]bool{}
	ast.Inspect(runFuncDecl(t).Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SwitchStmt:
			if x.Tag == nil || !isArgsZero(x.Tag) {
				return true
			}
			for _, stmt := range x.Body.List {
				for _, e := range stmt.(*ast.CaseClause).List {
					if value, ok := stringLit(e); ok {
						dispatched[value] = true
					}
				}
			}
		case *ast.BinaryExpr:
			if x.Op == token.EQL && isArgsZero(x.X) {
				if value, ok := stringLit(x.Y); ok {
					dispatched[value] = true
				}
			}
		}
		return true
	})
	completed := map[string]bool{}
	for _, name := range parseCompletionHelp(completionRoleHelp["superuser"]).root.childNames() {
		completed[name] = true
	}
	for _, name := range completionInternal {
		if completed[name] {
			t.Errorf("internal command %q is completed", name)
		}
		completed[name] = true
	}
	for name := range dispatched {
		if !completed[name] {
			t.Errorf("dispatched command %q is neither in help nor declared internal", name)
		}
	}
	for name := range completed {
		if !dispatched[name] {
			t.Errorf("completed command %q is not dispatched by run", name)
		}
	}
}

func controlSources(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// Subcommands come from help text; each must also be a literal the CLI
// compares against, which catches description words parsed as commands.
func TestCompletionSubcommandsAreDispatchLiterals(t *testing.T) {
	literals := map[string]bool{}
	for _, file := range controlSources(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if value, ok := stringLit(e); ok {
					literals[value] = true
				}
			}
			return true
		})
	}
	parseCompletionHelp(completionRoleHelp["superuser"]).root.walk(func(n *completionNode) {
		if strings.Contains(n.path, " ") && !literals[n.name()] && !literals[n.path] {
			t.Errorf("subcommand %q has no matching literal in the CLI sources", n.path)
		}
	})
}

// Every completed flag is defined by some flag set, and the global flags match
// the set run defines, including which take a value.
func TestCompletionFlagsAreDefined(t *testing.T) {
	first := map[string]bool{"Bool": true, "String": true, "Int": true, "Int64": true, "Uint": true, "Uint64": true, "Float64": true, "Duration": true, "Func": true, "BoolFunc": true}
	second := map[string]bool{"BoolVar": true, "StringVar": true, "IntVar": true, "Int64Var": true, "UintVar": true, "Uint64Var": true, "Float64Var": true, "DurationVar": true, "TextVar": true, "Var": true}
	defined := map[string]bool{}
	for _, file := range controlSources(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			index := -1
			if first[sel.Sel.Name] {
				index = 0
			} else if second[sel.Sel.Name] {
				index = 1
			}
			if index >= 0 && len(call.Args) > index {
				if name, ok := stringLit(call.Args[index]); ok {
					defined[name] = true
				}
			}
			return true
		})
	}
	s := parseCompletionHelp(completionRoleHelp["superuser"])
	s.root.walk(func(n *completionNode) {
		for _, flag := range n.flags {
			if !defined[strings.TrimPrefix(flag, "--")] {
				t.Errorf("%q completes undefined flag %s", n.path, flag)
			}
		}
	})
	var globals, values []string
	ast.Inspect(runFuncDecl(t).Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !first[sel.Sel.Name] || len(call.Args) == 0 {
			return true
		}
		if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "flags" {
			if name, ok := stringLit(call.Args[0]); ok {
				globals = append(globals, "--"+name)
				if sel.Sel.Name != "Bool" {
					values = append(values, "--"+name)
				}
			}
		}
		return true
	})
	if !slices.Equal(globals, s.root.flags) || !slices.Equal(values, s.valueGlobal) {
		t.Fatalf("global flags %v (values %v) differ from help %v (values %v)", globals, values, s.root.flags, s.valueGlobal)
	}
}

func writeCompletion(t *testing.T, shell, help string) string {
	t.Helper()
	script, err := completionScript(shell, help)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control."+shell)
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runShell(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// The real shells parse every role's script when they are installed.
func TestCompletionScriptSyntax(t *testing.T) {
	checks := map[string]func(path string) []string{
		"bash": func(path string) []string { return []string{"bash", "--noprofile", "--norc", "-n", path} },
		"zsh":  func(path string) []string { return []string{"zsh", "-f", "-n", path} },
		"fish": func(path string) []string { return []string{"fish", "--no-config", "--no-execute", path} },
		"powershell": func(path string) []string {
			check := filepath.Join(t.TempDir(), "check.ps1")
			script := `$errors = $null
[System.Management.Automation.Language.Parser]::ParseFile($args[0], [ref]$null, [ref]$errors) | Out-Null
if ($errors.Count -gt 0) { $errors | ForEach-Object { $_.ToString() }; exit 1 }
`
			if err := os.WriteFile(check, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"pwsh", "-NoProfile", "-NonInteractive", "-File", check, path}
		},
	}
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			argv := checks[shell]("")
			if _, err := exec.LookPath(argv[0]); err != nil {
				t.Skipf("%s is not installed", argv[0])
			}
			for _, help := range completionRoleHelp {
				argv := checks[shell](writeCompletion(t, shell, help))
				runShell(t, argv[0], argv[1:]...)
			}
		})
	}
}

type completionCase struct {
	words []string
	bash  string
	zsh   string
}

// The last word is the one being completed. bash filters by prefix itself;
// zsh's compadd filters later, so the zsh column lists everything offered.
var completionCases = []completionCase{
	{[]string{"control", "ta"}, "task", "ROOT"},
	{[]string{"control", "task", ""}, "start get wait cancel logs list reconcile", "start get wait cancel logs list reconcile"},
	{[]string{"control", "--config", "/x", "task", "st"}, "start", "start get wait cancel logs list reconcile"},
	{[]string{"control", "--debug", "tunnel", ""}, "start list dispose", "start list dispose"},
	{[]string{"control", "task", "start", "--w"}, "--wait", "--wait --spec"},
	{[]string{"control", "--tok"}, "--token", "--config --api --token --debug"},
	{[]string{"control", "tunnel", "start", "--l"}, "--listen", "--listen --reverse --id --ttl"},
	{[]string{"control", "completion", ""}, "bash zsh fish powershell", "bash zsh fish powershell"},
	{[]string{"control", "users", ""}, "", ""},
	{[]string{"control", "machines", ""}, "", "<files>"},
	{[]string{"control", "--config", ""}, "", "<files>"},
	{[]string{"control", "tunnel", "worker", ""}, "", "<files>"},
	{[]string{"control", "exec", "worker", "--", ""}, "", "<files>"},
}

func shellWords(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, "'"+word+"'")
	}
	return strings.Join(quoted, " ")
}

func TestBashCompletionFunction(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	var driver strings.Builder
	driver.WriteString(`source "$1"
c() { COMP_WORDS=("$@"); COMP_CWORD=$(( ${#COMP_WORDS[@]} - 1 )); COMPREPLY=(); _control; echo "${COMPREPLY[*]}"; }
`)
	for _, tc := range completionCases {
		driver.WriteString("c " + shellWords(tc.words) + "\n")
	}
	// Without bash-completion, COMP_WORDBREAKS splits --config=/x around '='.
	driver.WriteString("c control --config = /x tunnel ''\n")
	path := filepath.Join(t.TempDir(), "driver.bash")
	if err := os.WriteFile(path, []byte(driver.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(runShell(t, "bash", "--noprofile", "--norc", path, writeCompletion(t, "bash", usage)), "\n"), "\n")
	want := make([]string, 0, len(completionCases)+1)
	for _, tc := range completionCases {
		want = append(want, tc.bash)
	}
	want = append(want, "start list dispose")
	if !slices.Equal(lines, want) {
		t.Fatalf("bash completions\n got %q\nwant %q", lines, want)
	}
}

func TestZshCompletionFunction(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is not installed")
	}
	var driver strings.Builder
	// Outside a completion widget, stub the completion builtins and record
	// what _control offers.
	driver.WriteString(`compdef() { :; }
typeset -ga out
compadd() { [[ $1 == -- ]] && shift; out+=("$@"); }
_files() { out+=('<files>'); }
source "$1"
c() { local -a words; words=("$@"); local -i CURRENT=$#; out=(); _control; print -r -- "${out[*]}"; }
`)
	for _, tc := range completionCases {
		driver.WriteString("c " + shellWords(tc.words) + "\n")
	}
	driver.WriteString("c control --config=/x tunnel ''\n")
	path := filepath.Join(t.TempDir(), "driver.zsh")
	if err := os.WriteFile(path, []byte(driver.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(runShell(t, "zsh", "-f", path, writeCompletion(t, "zsh", usage)), "\n"), "\n")
	want := make([]string, 0, len(completionCases)+1)
	root := strings.Join(parseCompletionHelp(usage).root.childNames(), " ")
	for _, tc := range completionCases {
		want = append(want, strings.ReplaceAll(tc.zsh, "ROOT", root))
	}
	want = append(want, "start list dispose")
	if !slices.Equal(lines, want) {
		t.Fatalf("zsh completions\n got %q\nwant %q", lines, want)
	}
}
