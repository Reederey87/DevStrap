package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeGOOSBranchesStayInPlatformPackage(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "vendor" || name == ".devstrap" {
				return filepath.SkipDir
			}
			if rel == "internal/platform" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "GOOS" {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if ok && ident.Name == "runtime" {
				position := fileSet.Position(selector.Pos())
				offenders = append(offenders, position.String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("runtime.GOOS must stay behind internal/platform: %s", strings.Join(offenders, ", "))
	}
}
