package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

var codeFormat = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)+$`)

func TestCatalogIsWellFormed(t *testing.T) {
	cats := map[string]bool{CategoryTickets: true, CategoryMessages: true, CategoryContacts: true, CategoryUsers: true,
		CategoryGroups: true, CategoryTags: true, CategoryIntegrations: true, CategoryWebhooks: true,
		CategoryConfiguration: true, CategorySLA: true, CategorySecurity: true, CategorySystem: true}
	seen := map[Action]bool{}
	for _, a := range Actions() {
		if seen[a.Code] {
			t.Errorf("duplicate action %q", a.Code)
		}
		seen[a.Code] = true
		if !codeFormat.MatchString(string(a.Code)) || len(a.Code) > 100 {
			t.Errorf("action %q: code format", a.Code)
		}
		if a.Label == "" || !cats[a.Category] {
			t.Errorf("action %q: label %q, category %q", a.Code, a.Label, a.Category)
		}
	}
	if ActionLabel("ticket.created") != "Created a ticket" || ActionLabel("nope.never") != "nope.never" {
		t.Error("ActionLabel")
	}
	ids := map[string]bool{}
	for _, s := range SystemActors() {
		if ids[s.Id] || s.Name == "" || !KnownSystemActor(s.Id) {
			t.Errorf("system actor %+v", s)
		}
		ids[s.Id] = true
	}
	if SystemActorName("automation") != "System · Automation" || SystemActorName("") != "System" {
		t.Error("SystemActorName")
	}
}

// TestEveryActionIsCatalogued type-checks every package of the module and
// fails when a constant of type Action (a literal passed to audit.New, an
// Event's Action field, a helper taking an Action, a constant declaration)
// holds a code that is not in the catalog. Non-constant Action values cannot
// be built from strings without an explicit conversion, which no writer
// uses, so every written action is checked.
func TestEveryActionIsCatalogued(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks the whole module")
	}
	out, err := exec.Command("go", "list", "-export", "-deps", "-test=false",
		"-json=ImportPath,Export,Dir,GoFiles,Module", "github.com/arksnorman/open-kase/api/...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	type pkg struct {
		ImportPath, Export, Dir string
		GoFiles                 []string
		Module                  *struct{ Path string }
	}
	exports := map[string]string{}
	var own []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Path == "github.com/arksnorman/open-kase/api" {
			own = append(own, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	const auditPath = "github.com/arksnorman/open-kase/api/internal/audit"
	isAction := func(typ types.Type) bool {
		n, ok := typ.(*types.Named)
		return ok && n.Obj().Name() == "Action" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == auditPath
	}
	checked := 0
	for _, p := range own {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for expr, tv := range info.Types {
			if tv.Value == nil || tv.Value.Kind() != constant.String || !isAction(tv.Type) {
				continue
			}
			checked++
			if code := constant.StringVal(tv.Value); code != "" {
				if _, ok := LookupAction(code); !ok {
					t.Errorf("%s: audit action %q is not in the catalog (internal/audit/actions_catalog.go)", fset.Position(expr.Pos()), code)
				}
			}
		}
	}
	if checked < len(actions) {
		t.Fatalf("checked only %d Action constants; expected at least the %d catalogued ones", checked, len(actions))
	}
}
