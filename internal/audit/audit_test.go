package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// The API gives Actions to the web as an enum, and the web shows a text for
// each one. An action that is not in Actions shows as "did <action>".
func TestActionsHasEachAction(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "audit.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			spec := s.(*ast.ValueSpec)
			value, err := strconv.Unquote(spec.Values[0].(*ast.BasicLit).Value)
			if err != nil {
				t.Fatal(err)
			}
			found++
			if !slices.Contains(Actions, Action(value)) {
				t.Errorf("Actions does not have %s (%q)", spec.Names[0].Name, value)
			}
		}
	}
	if found != len(Actions) {
		t.Errorf("audit.go has %d actions, and Actions has %d", found, len(Actions))
	}
}
