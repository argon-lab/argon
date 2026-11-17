package walcli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Stdout belongs to whoever embeds the engine: "argon mcp" speaks JSON-RPC on
// it and "--output json" prints one JSON value there. Engine code that wants
// to warn must use stderr, so nothing under internal/ or pkg/ may print with
// fmt.Print, fmt.Printf or fmt.Println.
func TestEngineNeverPrintsToStdout(t *testing.T) {
	for _, root := range []string{"../../internal", "../../pkg"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files := token.NewFileSet()
			file, err := parser.ParseFile(files, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" {
					return true
				}
				switch selector.Sel.Name {
				case "Print", "Printf", "Println":
					t.Errorf("%s: fmt.%s writes to stdout; use fmt.Fprint* with os.Stderr", files.Position(call.Pos()), selector.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
