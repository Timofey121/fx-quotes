package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/Timofey121/fx-quotes"

func TestProductionImportsRespectCleanArchitectureBoundaries(t *testing.T) {
	root := moduleRoot(t)
	for _, rule := range []struct {
		name      string
		directory string
		allowed   func(string) bool
	}{
		{
			name:      "entity imports only standard library packages",
			directory: "internal/entity",
			allowed:   isStandardLibrary,
		},
		{
			name:      "use cases import only the standard library and entities",
			directory: "internal/usecase",
			allowed: func(path string) bool {
				return isStandardLibrary(path) || path == modulePath+"/internal/entity"
			},
		},
		{
			name:      "adapters do not import application wiring or other adapters",
			directory: "internal/adapter",
			allowed: func(path string) bool {
				return !strings.HasPrefix(path, modulePath+"/internal/app") && !strings.HasPrefix(path, modulePath+"/internal/adapter/")
			},
		},
	} {
		t.Run(rule.name, func(t *testing.T) {
			for _, file := range productionFiles(t, filepath.Join(root, rule.directory)) {
				for _, importPath := range imports(t, file) {
					if !rule.allowed(importPath) {
						t.Errorf("%s imports forbidden dependency %q", relativePath(t, root, file), importPath)
					}
				}
			}
		})
	}
}

func productionFiles(t *testing.T, directory string) []string {
	t.Helper()
	result := make([]string, 0)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		result = append(result, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func imports(t *testing.T, filename string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		result = append(result, unquoteImport(t, spec))
	}
	return result
}

func unquoteImport(t *testing.T, spec *ast.ImportSpec) string {
	t.Helper()
	value, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func isStandardLibrary(importPath string) bool {
	pkg, err := build.Default.Import(importPath, "", build.FindOnly)
	return err == nil && pkg.Goroot
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func relativePath(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}
