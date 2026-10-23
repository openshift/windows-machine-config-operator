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

// TestSpecTimeoutCallbackSignatures verifies that every Ginkgo interruptible
// node (It, BeforeEach, AfterEach, JustBeforeEach, JustAfterEach, BeforeAll,
// AfterAll) that uses a SpecTimeout, NodeTimeout, or GracePeriod decorator
// has a callback with exactly one parameter of type g.SpecContext or
// context.Context in the first position.
//
// Without this parameter Ginkgo panics during test discovery
// ("Invalid NodeTimeout SpecTimeout, or GracePeriod") and silently drops
// every spec in the suite.
//
// Container nodes (Describe, Context, When) are excluded — Ginkgo rejects
// timeout decorators on containers at a different validation layer.
//
// This is a regression test for the bug fixed in PR #4566 / OCP-68320.
func TestSpecTimeoutCallbackSignatures(t *testing.T) {
	// Decorators that require a context-accepting callback.
	timeoutDecorators := map[string]bool{
		"SpecTimeout": true,
		"NodeTimeout": true,
		"GracePeriod": true,
	}

	// All non-container interruptible node types that support timeout
	// decorators per the Ginkgo v2 contract (internal/node.go).
	// Container nodes (Describe, Context, When) are excluded because
	// Ginkgo rejects timeout decorators on them separately.
	interruptibleNodes := map[string]bool{
		"It":             true,
		"BeforeEach":     true,
		"AfterEach":      true,
		"JustBeforeEach": true,
		"JustAfterEach":  true,
		"BeforeAll":      true,
		"AfterAll":       true,
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

		// Resolve import aliases so we can match exact types.
		// e.g. g "github.com/onsi/ginkgo/v2" → alias "g"
		//      "context" → alias "context"
		ginkgoAlias := resolveImportAlias(f, "github.com/onsi/ginkgo/v2")
		contextAlias := resolveImportAlias(f, "context")

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Match g.It(...), g.BeforeEach(...), etc.
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !interruptibleNodes[sel.Sel.Name] {
				return true
			}

			if len(call.Args) < 2 {
				return true
			}

			// Check whether any argument is a timeout decorator.
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
				if !callbackAcceptsContext(funcLit, ginkgoAlias, contextAlias) {
					pos := fset.Position(funcLit.Pos())
					violations = append(violations, pos.String())
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf("found Ginkgo interruptible-node callbacks with "+
			"SpecTimeout/NodeTimeout/GracePeriod decorators that do not "+
			"accept exactly one parameter of type SpecContext or "+
			"context.Context.\nGinkgo requires the callback to accept a "+
			"context parameter when these decorators are used.\n"+
			"Violations at:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// resolveImportAlias returns the local alias for an import path.
// If the import has an explicit alias (e.g. g "github.com/onsi/ginkgo/v2"),
// it returns the alias. Otherwise it returns the last path element
// (e.g. "context" for "context", "ginkgo" for ".../ginkgo/v2").
// Returns "" if the import is not found.
func resolveImportAlias(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		// No explicit alias — use last path element.
		parts := strings.Split(path, "/")
		return parts[len(parts)-1]
	}
	return ""
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

// callbackAcceptsContext returns true if the function literal has exactly
// one parameter and that parameter's type is either <ginkgoAlias>.SpecContext
// or <contextAlias>.Context. This matches the Ginkgo v2 contract which
// requires exactly func() or func(SpecContext)/func(context.Context) — no
// other parameter counts are accepted for interruptible bodies.
func callbackAcceptsContext(fn *ast.FuncLit, ginkgoAlias, contextAlias string) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}

	// Ginkgo's extractBodyFunction rejects callbacks with >1 parameter
	// or any return values. Count total parameters (a field may declare
	// multiple names, e.g. "a, b int").
	totalParams := 0
	for _, field := range fn.Type.Params.List {
		names := len(field.Names)
		if names == 0 {
			names = 1 // unnamed parameter
		}
		totalParams += names
	}
	if totalParams != 1 {
		return false
	}

	// Check the first (and only) parameter type.
	firstParam := fn.Type.Params.List[0]
	return isExactContextType(firstParam.Type, ginkgoAlias, contextAlias)
}

// isExactContextType returns true if the type expression is exactly
// <ginkgoAlias>.SpecContext or <contextAlias>.Context.
func isExactContextType(expr ast.Expr, ginkgoAlias, contextAlias string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}

	if ginkgoAlias != "" && ident.Name == ginkgoAlias && sel.Sel.Name == "SpecContext" {
		return true
	}
	if contextAlias != "" && ident.Name == contextAlias && sel.Sel.Name == "Context" {
		return true
	}
	return false
}
