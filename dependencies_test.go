package pgstore

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type goModEditJSON struct {
	Require []struct {
		Path     string
		Indirect bool
	}
	Replace []json.RawMessage
}

func TestDependencyBoundary(t *testing.T) {
	t.Parallel()

	command := exec.Command("go", "mod", "edit", "-json")
	command.Env = append(command.Environ(), "GOWORK=off")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var mod goModEditJSON
	if err := json.Unmarshal(output, &mod); err != nil {
		t.Fatalf("decode go.mod: %v", err)
	}
	if len(mod.Replace) != 0 {
		t.Fatalf("go.mod has %d replace directives, want none", len(mod.Replace))
	}

	var direct []string
	for _, requirement := range mod.Require {
		if !requirement.Indirect {
			direct = append(direct, requirement.Path)
		}
	}
	slices.Sort(direct)
	wantDirect := []string{"github.com/jackc/pgx/v5", "github.com/looprig/storage"}
	if !slices.Equal(direct, wantDirect) {
		t.Fatalf("direct modules = %v, want %v", direct, wantDirect)
	}

	var paths []string
	err = filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk production files: %v", err)
	}
	parsed := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", path, err)
			}
			if importPath == "log" || importPath == "log/slog" {
				t.Errorf("%s imports logging package %q; pgstore must not create a path for DSN or credentials to reach logs", path, importPath)
			}
			if strings.Contains(importPath, ".") &&
				!strings.HasPrefix(importPath, "github.com/jackc/pgx/v5") &&
				importPath != "github.com/looprig/storage" &&
				!strings.HasPrefix(importPath, "github.com/looprig/pgstore/internal/") {
				t.Errorf("%s imports unapproved package %q", path, importPath)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no production Go files parsed")
	}
}
