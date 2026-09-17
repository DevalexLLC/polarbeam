package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the module root relative to this package directory.
const repoRoot = "../.."

// eventConstants parses catalog.go and returns every Event* constant's
// name and value, so the call-site scan can check both that a constant
// exists and that the catalog map lists it.
func eventConstants(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "catalog.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Event") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", n.Name)
				}
				out[n.Name] = strings.Trim(lit.Value, `"`)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Event constants parsed from catalog.go")
	}
	return out
}

func TestCatalogListsEveryConstant(t *testing.T) {
	consts := eventConstants(t)
	seen := map[string]bool{}
	for name, id := range consts {
		if _, ok := Catalog[id]; !ok {
			t.Errorf("%s (%q) has no Catalog entry", name, id)
		}
		if seen[id] {
			t.Errorf("event id %q is declared twice", id)
		}
		seen[id] = true
	}
	for id := range Catalog {
		if !seen[id] {
			t.Errorf("Catalog entry %q has no Event constant", id)
		}
	}
}

// goSources walks cmd/ and internal/ for non-test Go files outside this
// package.
func goSources(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "audit" && filepath.Base(filepath.Dir(path)) == "internal" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no Go sources found — wrong repoRoot?")
	}
	return files
}

// TestReservedEventKeyIsAuditOnly: a record whose first attribute is
// "event" is an audit record by construction, so no other package may log
// under that key.
func TestReservedEventKeyIsAuditOnly(t *testing.T) {
	re := regexp.MustCompile(`"event"`)
	for _, path := range goSources(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d logs under the reserved key \"event\": use audit.Emit or another key", path, i+1)
			}
		}
	}
}

// TestEmitCallSitesUseCatalogConstants: every audit.Event literal in the
// tree names its ID through an Event* constant that exists — never a
// string literal, so the catalog (and the operator doc generated from
// it) can never drift from what the code emits.
func TestEmitCallSitesUseCatalogConstants(t *testing.T) {
	consts := eventConstants(t)
	constRef := regexp.MustCompile(`\bID:\s*audit\.(Event[A-Za-z0-9]+)`)
	literal := regexp.MustCompile(`\bID:\s*"`)
	calls := 0
	for _, path := range goSources(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "audit.Event{") && !strings.Contains(string(src), "audit.Event ") {
			continue
		}
		for i, line := range strings.Split(string(src), "\n") {
			if literal.MatchString(line) && strings.Contains(line, "audit") {
				t.Errorf("%s:%d names an audit event by string literal; use an audit.Event* constant", path, i+1)
			}
			for _, m := range constRef.FindAllStringSubmatch(line, -1) {
				calls++
				if _, ok := consts[m[1]]; !ok {
					t.Errorf("%s:%d references audit.%s, which does not exist", path, i+1, m[1])
				}
			}
		}
	}
	if calls == 0 {
		t.Error("no audit.Event call sites found — scan broken?")
	}
}

// TestOperatorDocListsEveryEvent: docs/audit-logging.md is the operator's
// copy of the catalog; an event added here without a documented trigger
// fails.
func TestOperatorDocListsEveryEvent(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot, "docs", "audit-logging.md"))
	if err != nil {
		t.Fatal(err)
	}
	for id := range Catalog {
		if !strings.Contains(string(doc), "`"+id+"`") {
			t.Errorf("docs/audit-logging.md does not document event %q", id)
		}
	}
}
