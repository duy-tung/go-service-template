package usecase

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUsecaseImportHygiene enforces the dependency rule: the use case layer
// must not import database/sql, drivers, pkg/xsql, or transport packages.
func TestUsecaseImportHygiene(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	forbidden := []string{
		"database/sql",
		"github.com/jackc/pgx",
		"github.com/acme/order-engine/pkg/xsql",
		"github.com/acme/order-engine/pkg/dataservicex",
		"github.com/acme/order-engine/internal/transport",
		"github.com/acme/order-engine/internal/repository",
	}

	for _, pkg := range pkgs {
		for fileName, file := range pkg.Files {
			if strings.HasSuffix(fileName, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				for _, banned := range forbidden {
					if path == banned || strings.HasPrefix(path, banned+"/") {
						t.Errorf("%s imports forbidden package %s", fileName, path)
					}
				}
			}
		}
	}
}
