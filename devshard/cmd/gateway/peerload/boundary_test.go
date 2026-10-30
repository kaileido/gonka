package peerload

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const gatewayImportPath = "devshard/cmd/gateway"

func gatewayFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return walkErr
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(path)] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func packagesReaching(target string, files map[string]*ast.File) []string {
	importers := map[string][]string{}
	for path, file := range files {
		owner := gatewayImportPath + strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "..")
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			importers[imported] = append(importers[imported], owner)
		}
	}
	reaching := map[string]struct{}{}
	pending := []string{target}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, owner := range importers[next] {
			if _, seen := reaching[owner]; !seen {
				reaching[owner] = struct{}{}
				pending = append(pending, owner)
			}
		}
	}
	return slices.Sorted(maps.Keys(reaching))
}

// Test flow:
//  1. Parse every production file of the gateway.
//  2. Assert the only gateway packages that reach peerload through their imports are the binary's composition, app and metrics, so no package on the request path can read it.
//  3. Assert the only call of View outside peerload is in the metrics collector.
func TestOnlyTheMetricsCollectorReadsThePeerView(t *testing.T) {
	files := gatewayFiles(t)

	want := []string{gatewayImportPath, gatewayImportPath + "/app", gatewayImportPath + "/metrics"}
	if got := packagesReaching(gatewayImportPath+"/peerload", files); !slices.Equal(got, want) {
		t.Fatalf("packages reaching peerload = %v, want %v", got, want)
	}

	var readers []string
	for path, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && selector.Sel.Name == "View" {
				readers = append(readers, path)
			}
			return true
		})
	}
	readers = slices.DeleteFunc(readers, func(path string) bool { return strings.HasPrefix(path, "../peerload/") })
	if !slices.Equal(readers, []string{"../metrics/peerload_collector.go"}) {
		t.Fatalf("View is read by %v, want only the metrics collector", readers)
	}
}
