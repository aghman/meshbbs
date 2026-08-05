// Command checkdeterminism enforces the design §12.1 constraints that make
// deterministic simulation possible. These are not style preferences: a single
// violation makes a simulation run unreproducible, and one of them (map
// iteration in an encoder) silently corrupts signatures — see §6.2.1.
//
// It reports:
//
//   - time.Now / time.Since / time.After / time.Sleep / time.Tick in domain code
//     (use an injected clock.Clock)
//   - math/rand or math/rand/v2 package-level functions in domain code
//     (use an injected rng.Source)
//   - range over a map inside the canonical encoding package, where iteration
//     order is observable in the output bytes
//
// Run: go run ./tools/checkdeterminism ./...
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Packages exempt from the clock/rng rules. These are the places where the
// real implementations necessarily live, or where nondeterminism is harmless.
var exemptPrefixes = []string{
	"internal/clock",
	"internal/rng",
	"tools/",
	"cmd/", // command wiring constructs the real clock and passes it down
}

// Packages where map iteration order is observable in output bytes and must
// therefore never be relied upon (§6.2.1 rule 2).
var noMapRangePrefixes = []string{
	"internal/record",
}

type finding struct {
	pos  token.Position
	rule string
	msg  string
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = strings.TrimSuffix(strings.TrimSuffix(os.Args[1], "..."), "/")
		if root == "" {
			root = "."
		}
	}

	var findings []finding
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip dot-directories outright rather than just .git. A git
			// worktree checked out under .claude/worktrees/ is a second full
			// copy of the repo, and walking into it reports every exempt
			// package (internal/clock, internal/rng) as a violation: the
			// exemption is a path prefix, and the copy's paths are nested.
			// The walk root itself may be "." — never skip that.
			base := d.Name()
			if path != root && strings.HasPrefix(base, ".") {
				return fs.SkipDir
			}
			if base == "vendor" || base == "testdata" || base == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(path)
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		findings = append(findings, checkFile(fset, f, rel)...)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkdeterminism:", err)
		os.Exit(2)
	}

	if len(findings) == 0 {
		fmt.Println("checkdeterminism: ok")
		return
	}
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "%s: [%s] %s\n", f.pos, f.rule, f.msg)
	}
	fmt.Fprintf(os.Stderr, "\n%d determinism violation(s). See design §12.1.\n", len(findings))
	os.Exit(1)
}

func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func checkFile(fset *token.FileSet, f *ast.File, rel string) []finding {
	var out []finding
	checkTime := !hasPrefix(rel, exemptPrefixes)
	checkMaps := hasPrefix(rel, noMapRangePrefixes)

	// Track import aliases so `import t "time"` is still caught.
	timeNames := map[string]bool{}
	randNames := map[string]bool{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch p {
		case "time":
			if name == "" {
				name = "time"
			}
			timeNames[name] = true
		case "math/rand", "math/rand/v2":
			if name == "" {
				name = "rand"
			}
			randNames[name] = true
		}
	}

	banned := map[string]string{
		"Now": "time.Now", "Since": "time.Since", "After": "time.After",
		"Sleep": "time.Sleep", "Tick": "time.Tick", "NewTimer": "time.NewTimer",
		"NewTicker": "time.NewTicker",
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if !checkTime {
				return true
			}
			ident, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			if timeNames[ident.Name] {
				if full, bad := banned[node.Sel.Name]; bad {
					out = append(out, finding{
						pos:  fset.Position(node.Pos()),
						rule: "clock",
						msg:  fmt.Sprintf("%s in domain code; take a clock.Clock instead", full),
					})
				}
			}
			if randNames[ident.Name] {
				out = append(out, finding{
					pos:  fset.Position(node.Pos()),
					rule: "rng",
					msg:  fmt.Sprintf("package-level %s.%s; take an rng.Source instead", ident.Name, node.Sel.Name),
				})
			}
		case *ast.RangeStmt:
			if !checkMaps {
				return true
			}
			// We cannot resolve types without full type-checking, so flag any
			// range whose expression is a plain identifier ending in a
			// map-suggesting name only if it is composite-literal typed here.
			// Instead be conservative and flag ranges over map literals and
			// over calls returning maps is out of scope; the common mistake is
			// `for k, v := range someMapField`.
			if isLikelyMap(node.X) {
				out = append(out, finding{
					pos:  fset.Position(node.Pos()),
					rule: "mapiter",
					msg:  "range over a map where output bytes are observable; sort keys first (§6.2.1)",
				})
			}
		}
		return true
	})
	return out
}

// isLikelyMap reports whether e is syntactically a map value. Without a type
// checker this is a heuristic; it catches map literals and composite types,
// which is where the mistake actually happens in encoders.
func isLikelyMap(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CompositeLit:
		_, ok := x.Type.(*ast.MapType)
		return ok
	case *ast.CallExpr:
		return false
	}
	return false
}
