package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errResult mirrors the production Commit call-site pattern used after
// root-c1q3p changed commitWorkingSet's zero-dirty case from nil to
// "nothing to commit".
func commitIgnoreNothing(err error) error {
	if err != nil && !isDoltNothingToCommit(err) {
		return fmt.Errorf("commit import: %w", err)
	}
	return nil
}

// TestBootstrapImportCommitIgnoresNothingToCommit is the Gate3 Major
// right-reason regression for cmd/bd/bootstrap.go's post-JSONL Commit:
// an empty / already-imported working set must not hard-fail bootstrap.
func TestBootstrapImportCommitIgnoresNothingToCommit(t *testing.T) {
	if err := commitIgnoreNothing(errors.New("nothing to commit")); err != nil {
		t.Fatalf("nothing to commit must be ignored, got %v", err)
	}
	if err := commitIgnoreNothing(errors.New("No changes to commit")); err != nil {
		t.Fatalf("No changes to commit must be ignored, got %v", err)
	}
	if err := commitIgnoreNothing(nil); err != nil {
		t.Fatalf("nil must pass, got %v", err)
	}
	err := commitIgnoreNothing(errors.New("disk full"))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("real errors must propagate, got %v", err)
	}
	if !strings.Contains(err.Error(), "commit import:") {
		t.Fatalf("error must keep bootstrap wrap prefix, got %v", err)
	}
}

// TestCmdBdDoltCommitCallSitesGuardNothingToCommit walks production
// cmd/bd/*.go (excluding *_test.go) and fails if any store/uow
// Commit/CommitWithConfig call site lacks isDoltNothingToCommit in the
// surrounding statement. This locks the Gate3 caller-audit requirement.
func TestCmdBdDoltCommitCallSitesGuardNothingToCommit(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Tests may run from module root or package dir depending on go test invocation.
	pkgDir := dir
	if _, err := os.Stat(filepath.Join(dir, "bootstrap.go")); err != nil {
		pkgDir = filepath.Join(dir, "cmd", "bd")
	}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var unguarded []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Commit" && sel.Sel.Name != "CommitWithConfig" {
				return true
			}
			// Ignore database/sql Tx.Commit() — zero args.
			if sel.Sel.Name == "Commit" && len(call.Args) == 0 {
				return true
			}
			pos := fset.Position(call.Pos())
			start := pos.Offset
			if start < 0 {
				return true
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			// Inspect a window around the call for the shared guard.
			lo := start - 200
			if lo < 0 {
				lo = 0
			}
			hi := start + 250
			if hi > len(src) {
				hi = len(src)
			}
			window := string(src[lo:hi])
			if !strings.Contains(window, "isDoltNothingToCommit") {
				unguarded = append(unguarded, fmt.Sprintf("%s:%d", name, pos.Line))
			}
			return true
		})
	}
	if len(unguarded) > 0 {
		t.Fatalf("Commit/CommitWithConfig call sites missing isDoltNothingToCommit guard:\n  %s", strings.Join(unguarded, "\n  "))
	}
}
