package node

import (
	"bufio"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/koltyakov/control"

// Code and documentation intentionally differ only for these entries. Every
// exception still has to be named in docs/protocol.md.
var (
	// JSON methods dispatched by the node but documented in prose next to
	// their streaming protocol rather than as rows of the method table.
	proseDocumentedMethods = map[string]string{
		"files.upload.status": "owner-scoped upload call documented beside files.upload.open",
		"files.upload.abort":  "owner-scoped upload call documented beside files.upload.open",
	}
	// Capabilities registered only when their configuration is present, so a
	// default node does not advertise them. Their registration must still exist
	// in this package's source.
	configuredCapabilities = map[string]string{
		"desktop.run": "registered only with desktop.library, desktop.command or desktop.cli",
		"browser.run": "registered only with browser.cli; documented in the browser section",
	}
)

// repositoryRoot finds the module root from the package directory, so the test
// does not depend on a fixed relative depth.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if first, _, _ := strings.Cut(string(data), "\n"); strings.TrimSpace(first) == "module "+modulePath {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root with go.mod not found")
		}
		dir = parent
	}
}

// packageConstants returns the string constants declared by one repository
// package, keyed by name, from its non-test sources.
func packageConstants(t *testing.T, dir string) map[string]string {
	t.Helper()
	constants := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, ident := range value.Names {
					if i < len(value.Values) {
						if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							constants[ident.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	return constants
}

// methodResolver turns string literals and package-qualified constants in a
// node source file into method names.
type methodResolver struct {
	t       *testing.T
	root    string
	imports map[string]string
	cache   map[string]map[string]string
}

func (r *methodResolver) resolve(expr ast.Expr) string {
	r.t.Helper()
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			value, err := strconv.Unquote(e.Value)
			if err == nil {
				return value
			}
		}
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok {
			path, ok := r.imports[pkg.Name]
			if ok && strings.HasPrefix(path, modulePath+"/") {
				if r.cache[path] == nil {
					r.cache[path] = packageConstants(r.t, filepath.Join(r.root, filepath.FromSlash(strings.TrimPrefix(path, modulePath+"/"))))
				}
				if value, ok := r.cache[path][e.Sel.Name]; ok {
					return value
				}
			}
		}
	}
	r.t.Fatalf("cannot resolve method expression %T at %v; extend protocol_docs_test.go", expr, expr.Pos())
	return ""
}

// nodeMethods parses node.go and returns the JSON methods handled by the
// dispatch switch and the methods the stream handler serves before dispatch.
func nodeMethods(t *testing.T, root string) (dispatched, streamed []string) {
	t.Helper()
	path := filepath.Join(root, "internal", "node", "node.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &methodResolver{t: t, root: root, imports: map[string]string{}, cache: map[string]map[string]string{}}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		name := importPath[strings.LastIndex(importPath, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		resolver.imports[name] = importPath
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		switch fn.Name.Name {
		case "dispatch":
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				sw, ok := node.(*ast.SwitchStmt)
				if !ok {
					return true
				}
				if tag, ok := sw.Tag.(*ast.Ident); !ok || tag.Name != "method" {
					return true
				}
				for _, stmt := range sw.Body.List {
					for _, expr := range stmt.(*ast.CaseClause).List {
						dispatched = append(dispatched, resolver.resolve(expr))
					}
				}
				return false
			})
		case "handle":
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				binary, ok := node.(*ast.BinaryExpr)
				if !ok || binary.Op != token.EQL {
					return true
				}
				if selector, ok := binary.X.(*ast.SelectorExpr); ok && selector.Sel.Name == "Method" {
					if request, ok := selector.X.(*ast.Ident); ok && request.Name == "request" {
						streamed = append(streamed, resolver.resolve(binary.Y))
					}
				}
				return true
			})
		}
	}
	if len(dispatched) < 20 || len(streamed) < 5 {
		t.Fatalf("parsed only %d dispatched and %d streamed methods; update the parser for node.go", len(dispatched), len(streamed))
	}
	return dispatched, streamed
}

var tableMethod = regexp.MustCompile("^\\|\\s*`([a-z][a-zA-Z0-9]*(?:\\.[a-zA-Z0-9]+)+)`\\s*\\|")

// protocolMethodTable returns the method names in the first column of the
// "### Node operations" table, and the whole protocol document.
func protocolMethodTable(t *testing.T, root string) ([]string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	inSection := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			if inSection {
				break
			}
			inSection = strings.TrimSpace(line) == "### Node operations"
			continue
		}
		if inSection {
			if match := tableMethod.FindStringSubmatch(line); match != nil {
				methods = append(methods, match[1])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(methods) < 20 {
		t.Fatalf("found %d rows in the docs/protocol.md Node operations table; did the heading change?", len(methods))
	}
	return methods, string(data)
}

func backticked(document, name string) bool { return strings.Contains(document, "`"+name+"`") }

// TestProtocolMethodTableMatchesNode fails when docs/protocol.md and the node's
// built-in capabilities or dispatched methods drift apart.
func TestProtocolMethodTableMatchesNode(t *testing.T) {
	root := repositoryRoot(t)
	n, err := New(Config{Name: "docs", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	var capabilities []string
	for _, capability := range n.Capabilities() {
		capabilities = append(capabilities, capability.Name)
	}
	dispatched, streamed := nodeMethods(t, root)
	table, document := protocolMethodTable(t, root)
	documented := map[string]bool{}
	for _, method := range table {
		if documented[method] {
			t.Errorf("docs/protocol.md lists %s twice in the Node operations table", method)
		}
		documented[method] = true
	}

	var missing []string
	for _, method := range slices.Concat(capabilities, dispatched) {
		if documented[method] {
			if _, exempt := proseDocumentedMethods[method]; exempt {
				t.Errorf("%s is now in the method table; remove it from proseDocumentedMethods", method)
			}
			continue
		}
		if _, exempt := proseDocumentedMethods[method]; exempt && backticked(document, method) {
			continue
		}
		missing = append(missing, method)
	}
	for _, method := range streamed {
		if !backticked(document, method) {
			missing = append(missing, method+" (streaming)")
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		t.Errorf("methods implemented by the node but missing from docs/protocol.md \"Node operations\": %s", strings.Join(slices.Compact(missing), ", "))
	}

	implemented := map[string]bool{}
	for _, method := range slices.Concat(capabilities, dispatched, streamed) {
		implemented[method] = true
	}
	source, err := nodeSource(root)
	if err != nil {
		t.Fatal(err)
	}
	for method, reason := range configuredCapabilities {
		if implemented[method] {
			t.Errorf("%s is registered by a default node; remove it from configuredCapabilities", method)
		}
		if !strings.Contains(source, `Name: "`+method+`"`) {
			t.Errorf("configured capability %s (%s) is no longer registered in internal/node", method, reason)
		}
		if !backticked(document, method) {
			t.Errorf("configured capability %s is not documented in docs/protocol.md", method)
		}
		implemented[method] = true
	}
	var stale []string
	for _, method := range table {
		if !implemented[method] {
			stale = append(stale, method)
		}
	}
	if len(stale) != 0 {
		t.Errorf("docs/protocol.md \"Node operations\" lists methods the node does not implement: %s", strings.Join(stale, ", "))
	}
}

// nodeSource concatenates this package's non-test sources.
func nodeSource(root string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(root, "internal", "node", "*.go"))
	if err != nil {
		return "", err
	}
	var source strings.Builder
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		source.Write(data)
	}
	if source.Len() == 0 {
		return "", errors.New("no node sources found")
	}
	return source.String(), nil
}
