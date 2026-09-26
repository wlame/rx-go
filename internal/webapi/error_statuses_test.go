package webapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// errorConstructorStatus maps each error constructor a handler can
// return through to the HTTP status it produces. A new constructor in
// errors.go needs a row here, or the walk below cannot see what it
// returns.
var errorConstructorStatus = map[string]int{
	"ErrBadRequest":         http.StatusBadRequest,
	"ErrInvalidRegex":       http.StatusBadRequest,
	"ErrForbidden":          http.StatusForbidden,
	"NewSandboxError":       http.StatusForbidden,
	"ErrNotFound":           http.StatusNotFound,
	"ErrConflict":           http.StatusConflict,
	"ErrInternal":           http.StatusInternalServerError,
	"ErrServiceUnavailable": http.StatusServiceUnavailable,
}

// TestOperations_DeclareEveryErrorStatusTheirHandlersReturn reads the
// package source, follows each operation's handler through the package
// functions it calls, and collects the statuses of the error
// constructors it reaches. huma adds its own: 400 for a body that is not
// JSON and 422 for one that fails the schema, and 422 for a missing or
// malformed query parameter. Every status found must be declared on the
// operation in the OpenAPI document, with a JSON body schema.
func TestOperations_DeclareEveryErrorStatusTheirHandlersReturn(t *testing.T) {
	returned := statusesReturnedByHandlers(t)
	if len(returned) == 0 {
		t.Fatal("found no huma.Register call in the package source")
	}

	doc := NewServer(Config{AppVersion: "error-statuses-test"}).API().OpenAPI()
	operations := map[string]*huma.Operation{}
	for _, item := range doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op != nil {
				operations[op.OperationID] = op
			}
		}
	}

	for operationID, statuses := range returned {
		op, ok := operations[operationID]
		if !ok {
			t.Errorf("operation %q is registered in the source but not in the document", operationID)
			continue
		}
		for status := range statusesHumaReturns(op) {
			statuses[status] = true
		}
		for _, status := range sortedStatuses(statuses) {
			response, declared := op.Responses[strconv.Itoa(status)]
			if !declared {
				t.Errorf("%s can answer %d but does not declare it (declares %v)",
					operationID, status, sortedKeys(op.Responses))
				continue
			}
			media := response.Content["application/json"]
			if media == nil || media.Schema == nil {
				t.Errorf("%s declares %d without a JSON body schema", operationID, status)
			}
		}
	}
}

// statusesHumaReturns lists the statuses huma itself answers for op
// before its handler runs.
func statusesHumaReturns(op *huma.Operation) map[int]bool {
	out := map[int]bool{}
	if op.RequestBody != nil {
		out[http.StatusBadRequest] = true
		out[http.StatusUnprocessableEntity] = true
	}
	for _, param := range op.Parameters {
		if param.In != "query" {
			continue
		}
		if param.Required || param.Schema == nil || param.Schema.Type != "string" {
			out[http.StatusUnprocessableEntity] = true
		}
	}
	return out
}

// statusesReturnedByHandlers maps each operation ID registered in the
// package source to the statuses of the error constructors its handler
// reaches, directly or through package-level functions it calls.
func statusesReturnedByHandlers(t *testing.T) map[string]map[int]bool {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}

	funcs := map[string]*ast.FuncDecl{}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}

	out := map[string]map[int]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isHumaRegister(call) || len(call.Args) != 3 {
				return true
			}
			operationID := operationIDOf(call.Args[1])
			if operationID == "" {
				t.Errorf("huma.Register at %s has no literal OperationID", fset.Position(call.Pos()))
				return true
			}
			statuses := map[int]bool{}
			collectStatuses(call.Args[2], funcs, map[string]bool{}, statuses)
			out[operationID] = statuses
			return true
		})
	}
	return out
}

// collectStatuses walks node, recording the status of every error
// constructor called and descending into every package-level function
// called that has not been visited yet.
func collectStatuses(node ast.Node, funcs map[string]*ast.FuncDecl, visited map[string]bool, statuses map[int]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if status, isConstructor := errorConstructorStatus[ident.Name]; isConstructor {
			statuses[status] = true
			return true
		}
		if fn, isFunc := funcs[ident.Name]; isFunc && !visited[ident.Name] && fn.Body != nil {
			visited[ident.Name] = true
			collectStatuses(fn.Body, funcs, visited, statuses)
		}
		return true
	})
	// A handler passed by name rather than as a literal.
	if ident, ok := node.(*ast.Ident); ok {
		if fn, isFunc := funcs[ident.Name]; isFunc && !visited[ident.Name] {
			visited[ident.Name] = true
			collectStatuses(fn.Body, funcs, visited, statuses)
		}
	}
}

// isHumaRegister reports whether call is huma.Register(...).
func isHumaRegister(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Register" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "huma"
}

// operationIDOf reads the OperationID string literal of a
// huma.Operation{...} composite literal.
func operationIDOf(expr ast.Expr) string {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "OperationID" {
			if value, ok := kv.Value.(*ast.BasicLit); ok {
				id, err := strconv.Unquote(value.Value)
				if err == nil {
					return id
				}
			}
		}
	}
	return ""
}

func sortedStatuses(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for status := range set {
		out = append(out, status)
	}
	sort.Ints(out)
	return out
}

func sortedKeys(responses map[string]*huma.Response) []string {
	out := make([]string, 0, len(responses))
	for key := range responses {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
