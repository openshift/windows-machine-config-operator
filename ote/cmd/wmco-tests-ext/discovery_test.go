package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpecTimeoutCallbackSignatures verifies that every g.It / g.Describe
// callback that uses g.SpecTimeout, g.NodeTimeout, or g.GracePeriod
// decorators has a function parameter accepting g.SpecContext or
// context.Context. Without this parameter Ginkgo panics during test
// discovery ("Invalid NodeTimeout SpecTimeout, or GracePeriod") and
// silently drops every spec in the suite.
//
// This is a regression test for the bug fixed in PR #4566 / OCP-68320.
func TestSpecTimeoutCallbackSignatures(t *testing.T) {
	// Decorators that require a context-accepting callback.
	timeoutDecorators := map[string]bool{
		"SpecTimeout": true,
		"NodeTimeout": true,
		"GracePeriod": true,
	}

	testDir := filepath.Join("..", "..", "test", "e2e")
	entries, err := os.ReadDir(testDir)
	if err != nil {
		t.Fatalf("failed to read OTE test directory %s: %v", testDir, err)
	}

	fset := token.NewFileSet()
	var violations []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		filePath := filepath.Join(testDir, entry.Name())
		f, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", filePath, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Match g.It(...) calls — the selector g.It
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "It" {
				return true
			}

			if len(call.Args) < 2 {
				return true
			}

			// Check whether any argument is a decorator call
			// (g.SpecTimeout, g.NodeTimeout, g.GracePeriod).
			hasTimeoutDecorator := false
			for _, arg := range call.Args {
				if isDecoratorCall(arg, timeoutDecorators) {
					hasTimeoutDecorator = true
					break
				}
			}
			if !hasTimeoutDecorator {
				return true
			}

			// Find the callback function literal among arguments.
			for _, arg := range call.Args {
				funcLit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				if !callbackAcceptsContext(funcLit) {
					pos := fset.Position(funcLit.Pos())
					violations = append(violations, pos.String())
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf("found g.It callbacks with SpecTimeout/NodeTimeout/GracePeriod "+
			"decorators that do not accept a SpecContext or context.Context parameter.\n"+
			"Ginkgo requires the callback to accept a context parameter when these "+
			"decorators are used.\nViolations at:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// isDecoratorCall checks whether an AST expression is a call to one of the
// known timeout-related Ginkgo decorator functions (e.g. g.SpecTimeout(...)).
func isDecoratorCall(expr ast.Expr, decorators map[string]bool) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return decorators[sel.Sel.Name]
}

// callbackAcceptsContext returns true if the function literal has at least
// one parameter whose type name contains "SpecContext" or "Context".
func callbackAcceptsContext(fn *ast.FuncLit) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	for _, param := range fn.Type.Params.List {
		typeName := typeNameString(param.Type)
		if strings.Contains(typeName, "SpecContext") || strings.Contains(typeName, "Context") {
			return true
		}
	}
	return false
}

// typeNameString returns a simple string representation of a type expression.
func typeNameString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeNameString(t.X) + "." + t.Sel.Name
	default:
		return ""
	}
}
