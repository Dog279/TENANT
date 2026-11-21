package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every command Tenant starts goes through internal/procprotect (TEN-535).
// On Windows, once Protect has run, a pipe made outside procprotect.Ordinary
// cannot be opened by a user who is not an administrator ("pipe: Access is
// denied"), and a process started without procprotect.Child cannot make a
// pipe of its own. A test run does not show it: tests do not call Protect,
// and a CI runner is an administrator, whom the access list lets through.
// So the source is held to it here.

// execMethods are exec.Cmd's methods that start a process or make a pipe.
var execMethods = map[string]bool{"Start": true, "Run": true, "Output": true, "CombinedOutput": true,
	"StdinPipe": true, "StdoutPipe": true, "StderrPipe": true}

// calledInsideOrdinary are the functions whose only caller runs them inside
// procprotect.Ordinary, after procprotect.Child.
var calledInsideOrdinary = map[string]bool{
	"internal/sandbox/process.go:startProcessHere":    true,
	"internal/mcp/transport/stdio.go:newStdioProcess": true,
}

// notCommands are the calls that look like one of exec.Cmd's methods, in a
// file that imports os/exec, and are something else's.
var notCommands = map[string]bool{
	"cmd/tenant/commands.go:imsgResp.Start": true, // the iMessage responder
}

// bareSpawns lists, for one file's source, the process starts and pipes made
// outside procprotect.
func bareSpawns(rel string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	usesExec := false
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
			usesExec = true
		}
	}
	isCall := func(n ast.Node, pkg, name string) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg && sel.Sel.Name == name
	}
	var ordinary [][2]token.Pos // the spans of procprotect.Ordinary(...) calls
	ast.Inspect(f, func(n ast.Node) bool {
		if n != nil && isCall(n, "procprotect", "Ordinary") {
			ordinary = append(ordinary, [2]token.Pos{n.Pos(), n.End()})
		}
		return true
	})
	inside := func(p token.Pos) bool {
		for _, s := range ordinary {
			if s[0] <= p && p < s[1] {
				return true
			}
		}
		return false
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		name := ""
		if ok {
			name = fn.Name.Name
		}
		if calledInsideOrdinary[rel+":"+name] {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || inside(c.Pos()) {
				return true
			}
			what := ""
			if isCall(c, "os", "Pipe") {
				what = "os.Pipe()"
			} else if sel, ok := c.Fun.(*ast.SelectorExpr); ok && usesExec && execMethods[sel.Sel.Name] && len(c.Args) == 0 &&
				!notCommands[rel+":"+types.ExprString(c.Fun)] {
				what = "." + sel.Sel.Name + "()"
			}
			if what != "" {
				out = append(out, fmt.Sprintf("%s: %s outside internal/procprotect", fset.Position(c.Pos()), what))
			}
			return true
		})
	}
	return out, nil
}

func TestEveryCommandStartsThroughProcprotect(t *testing.T) {
	root, _ := moduleRoot(t)
	files := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if strings.HasPrefix(name, "_") || name == "testdata" || name == "procprotect" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			found, err := bareSpawns(filepath.ToSlash(rel), nil2(os.ReadFile(path)))
			if err != nil {
				return err
			}
			files++
			for _, v := range found {
				t.Errorf("%s: use procprotect.Start, Run, Output or CombinedOutput, or procprotect.Child with the call inside procprotect.Ordinary", v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 200 {
		t.Fatalf("checked only %d files; the walk is broken", files)
	}

	// The checker itself: it sees the bare forms, and lets the wrapped ones be.
	const sample = `package p

import (
	"os"
	"os/exec"

	"tenant/internal/procprotect"
)

func bad(c *exec.Cmd) {
	c.Run()
	exec.Command("x").Output()
	c.StdoutPipe()
	os.Pipe()
}

func good(c *exec.Cmd, other interface{ Run(int) }) {
	procprotect.Run(c)
	other.Run(1)
	procprotect.Ordinary(func() { c.StdinPipe(); os.Pipe(); c.Start() })
}
`
	got, err := bareSpawns("sample.go", sample)
	if err != nil || len(got) != 4 {
		t.Fatalf("the checker found %d bare spawns in its sample, want 4: %v %v", len(got), got, err)
	}
}

// nil2 drops ReadFile's error: the parser reports an unreadable source.
func nil2(b []byte, _ error) []byte { return b }
