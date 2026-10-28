package github

// The exec guard below is borrowed from saddle's internal/forge
// (nodirect_test.go, github.com/brandonapol/saddle), cut down to gh.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ghNames are the expressions that name the gh CLI.
var ghNames = map[string]bool{`"gh"`: true, "github.GH": true}

// directExecs lists the exec.Command and exec.CommandContext calls in f
// that start gh.
func directExecs(fset *token.FileSet, f *ast.File) []token.Position {
	var hits []token.Position
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return true
		}
		arg := map[string]int{"Command": 0, "CommandContext": 1}
		i, ok := arg[sel.Sel.Name]
		if !ok || len(call.Args) <= i {
			return true
		}
		if ghNames[exprString(call.Args[i])] {
			hits = append(hits, fset.Position(call.Pos()))
		}
		return true
	})
	return hits
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Value
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name
		}
	}
	return ""
}

// TestNoDirectGHExec enforces ADR 0001 §3: no package but internal/github
// starts gh itself.
func TestNoDirectGHExec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	self := filepath.Join(root, "internal", "github")
	fset := token.NewFileSet()
	var hits []string
	sawMain := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor" || p == self) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if p == filepath.Join(root, "cmd", "lasso", "main.go") {
			sawMain = true
		}
		for _, h := range directExecs(fset, f) {
			hits = append(hits, h.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawMain {
		t.Fatalf("never scanned cmd/lasso/main.go under %s; the walk is broken", root)
	}
	if len(hits) > 0 {
		t.Fatalf("these start gh outside internal/github; use a github.Runner instead:\n%s", strings.Join(hits, "\n"))
	}
}

func TestDirectExecsFlagsEachForm(t *testing.T) {
	src := `package x
import ("os/exec"; "context")
func f(ctx context.Context, name string) {
	exec.Command("gh", "pr", "list")
	exec.CommandContext(ctx, "gh", "api")
	exec.Command(github.GH)
	exec.Command("git", "status")
	exec.Command(name)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := directExecs(fset, f)
	var lines []int
	for _, h := range hits {
		lines = append(lines, h.Line)
	}
	if len(lines) != 3 || lines[0] != 4 || lines[1] != 5 || lines[2] != 6 {
		t.Fatalf("directExecs = %v, want lines 4, 5 and 6", hits)
	}
}
