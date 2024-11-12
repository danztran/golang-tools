// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golang

// This file defines the behavior of the "Add test for FUNC" command.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/gopls/internal/cache"
	"golang.org/x/tools/gopls/internal/cache/metadata"
	"golang.org/x/tools/gopls/internal/cache/parsego"
	"golang.org/x/tools/gopls/internal/protocol"
	goplsastutil "golang.org/x/tools/gopls/internal/util/astutil"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/typesinternal"
)

const testTmplString = `
func {{.TestFuncName}}(t *{{.TestingPackageName}}.T) {
  {{- /* Test cases struct declaration and empty initialization. */}}
  tests := []struct {
    name string // description of this test case

    {{- $commentPrinted := false }}
    {{- if and .Receiver .Receiver.Constructor}}
    {{- range .Receiver.Constructor.Args}}
    {{- if .Name}}
    {{- if not $commentPrinted}}
    // Named input parameters for receiver constructor.
    {{- $commentPrinted = true }}
    {{- end}}
    {{.Name}} {{.Type}}
    {{- end}}
    {{- end}}
    {{- end}}

    {{- $commentPrinted := false }}
    {{- range .Func.Args}}
    {{- if .Name}}
    {{- if not $commentPrinted}}
    // Named input parameters for target function.
    {{- $commentPrinted = true }}
    {{- end}}
    {{.Name}} {{.Type}}
    {{- end}}
    {{- end}}

    {{- range $index, $res := .Func.Results}}
    {{- if eq $res.Name "gotErr"}}
    wantErr bool
    {{- else if eq $index 0}}
    want {{$res.Type}}
    {{- else}}
    want{{add $index 1}} {{$res.Type}}
    {{- end}}
    {{- end}}
  }{
    // TODO: Add test cases.
  }

  {{- /* Loop over all the test cases. */}}
  for _, tt := range tests {
    t.Run(tt.name, func(t *{{.TestingPackageName}}.T) {
      {{- /* Constructor or empty initialization. */}}
      {{- if .Receiver}}
      {{- if .Receiver.Constructor}}
      {{- /* Receiver variable by calling constructor. */}}
      {{fieldNames .Receiver.Constructor.Results ""}} := {{if .PackageName}}{{.PackageName}}.{{end}}
      {{- .Receiver.Constructor.Name}}

      {{- /* Constructor input parameters. */ -}}
      (
        {{- range $index, $arg := .Receiver.Constructor.Args}}
        {{- if ne $index 0}}, {{end}}
        {{- if .Name}}tt.{{.Name}}{{else}}{{.Value}}{{end}}
        {{- end -}}
      )

      {{- /* Handles the error return from constructor. */}}
      {{- $last := last .Receiver.Constructor.Results}}
      {{- if eq $last.Type "error"}}
      if err != nil {
        t.Fatalf("could not contruct receiver type: %v", err)
      }
      {{- end}}
      {{- else}}
      {{- /* Receiver variable declaration. */}}
      // TODO: construct the receiver type.
      var {{.Receiver.Var.Name}} {{.Receiver.Var.Type}}
      {{- end}}
      {{- end}}

      {{- /* Got variables. */}}
      {{if .Func.Results}}{{fieldNames .Func.Results ""}} := {{end}}

      {{- /* Call expression. */}}
      {{- if .Receiver}}{{/* Call method by VAR.METHOD. */}}
      {{- .Receiver.Var.Name}}.
      {{- else if .PackageName}}{{/* Call function by PACKAGE.FUNC. */}}
      {{- .PackageName}}.
      {{- end}}{{.Func.Name}}

      {{- /* Input parameters. */ -}}
      (
        {{- range $index, $arg := .Func.Args}}
        {{- if ne $index 0}}, {{end}}
        {{- if .Name}}tt.{{.Name}}{{else}}{{.Value}}{{end}}
        {{- end -}}
      )

      {{- /* Handles the returned error before the rest of return value. */}}
      {{- $last := last .Func.Results}}
      {{- if eq $last.Type "error"}}
      if gotErr != nil {
        if !tt.wantErr {
          t.Errorf("{{$.Func.Name}}() failed: %v", gotErr)
        }
        return
      }
      if tt.wantErr {
        t.Fatal("{{$.Func.Name}}() succeeded unexpectedly")
      }
      {{- end}}

      {{- /* Compare the returned values except for the last returned error. */}}
      {{- if or (and .Func.Results (ne $last.Type "error")) (and (gt (len .Func.Results) 1) (eq $last.Type "error"))}}
      // TODO: update the condition below to compare got with tt.want.
      {{- range $index, $res := .Func.Results}}
      {{- if ne $res.Name "gotErr"}}
      if true {
        t.Errorf("{{$.Func.Name}}() = %v, want %v", {{.Name}}, tt.{{if eq $index 0}}want{{else}}want{{add $index 1}}{{end}})
      }
      {{- end}}
      {{- end}}
      {{- end}}
    })
  }
}
`

// Name is the name of the field this input parameter should reference.
// Value is the expression this input parameter should accept.
//
// Exactly one of Name or Value must be set.
type field struct {
	Name, Type, Value string
}

type function struct {
	Name    string
	Args    []field
	Results []field
}

type receiver struct {
	// Var is the name and type of the receiver variable.
	Var field
	// Constructor holds information about the constructor for the receiver type.
	// If no qualified constructor is found, this field will be nil.
	Constructor *function
}

type testInfo struct {
	// TestingPackageName is the package name should be used when referencing
	// package "testing"
	TestingPackageName string
	// PackageName is the package name the target function/method is delcared from.
	PackageName  string
	TestFuncName string
	// Func holds information about the function or method being tested.
	Func function
	// Receiver holds information about the receiver of the function or method
	// being tested.
	// This field is nil for functions and non-nil for methods.
	Receiver *receiver
}

var testTmpl = template.Must(template.New("test").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"last": func(slice []field) field {
		if len(slice) == 0 {
			return field{}
		}
		return slice[len(slice)-1]
	},
	"fieldNames": func(fields []field, qualifier string) (res string) {
		var names []string
		for _, f := range fields {
			names = append(names, qualifier+f.Name)
		}
		return strings.Join(names, ", ")
	},
}).Parse(testTmplString))

// AddTestForFunc adds a test for the function enclosing the given input range.
// It creates a _test.go file if one does not already exist.
func AddTestForFunc(ctx context.Context, snapshot *cache.Snapshot, loc protocol.Location) (changes []protocol.DocumentChange, _ error) {
	pkg, pgf, err := NarrowestPackageForFile(ctx, snapshot, loc.URI)
	if err != nil {
		return nil, err
	}

	if metadata.IsCommandLineArguments(pkg.Metadata().ID) {
		return nil, fmt.Errorf("current file in command-line-arguments package")
	}

	if errors := pkg.ParseErrors(); len(errors) > 0 {
		return nil, fmt.Errorf("package has parse errors: %v", errors[0])
	}
	if errors := pkg.TypeErrors(); len(errors) > 0 {
		return nil, fmt.Errorf("package has type errors: %v", errors[0])
	}

	type packageInfo struct {
		name    string
		renamed bool
	}

	var (
		// fileImports is a map contains all the path imported in the original
		// file foo.go.
		fileImports map[string]packageInfo
		// testImports is a map contains all the path already imported in test
		// file foo_test.go.
		testImports map[string]packageInfo
		// extraImportsis a map from package path to local package name that
		// need to be imported for the test function.
		extraImports = make(map[string]packageInfo)
	)

	var collectImports = func(file *ast.File) (map[string]packageInfo, error) {
		imps := make(map[string]packageInfo)
		for _, spec := range file.Imports {
			// TODO(hxjiang): support dot imports.
			if spec.Name != nil && spec.Name.Name == "." {
				return nil, fmt.Errorf("\"add a test for func\" does not support files containing dot imports")
			}
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if spec.Name != nil {
				if spec.Name.Name == "_" {
					continue
				}
				imps[path] = packageInfo{spec.Name.Name, true}
			} else {
				// The package name might differ from the base of its import
				// path. For example, "/path/to/package/foo" could declare a
				// package named "bar". Look up the target package ensures the
				// accurate package name reference.
				//
				// While it's best practice to rename imported packages when
				// their name differs from the base path (e.g.,
				// "import bar \"path/to/package/foo\""), this is not mandatory.
				id := pkg.Metadata().DepsByImpPath[metadata.ImportPath(path)]
				if metadata.IsCommandLineArguments(id) {
					return nil, fmt.Errorf("can not import command-line-arguments package")
				}
				if id == "" { // guess upon missing.
					imps[path] = packageInfo{imports.ImportPathToAssumedName(path), false}
				} else {
					fromPkg, ok := snapshot.MetadataGraph().Packages[id]
					if !ok {
						return nil, fmt.Errorf("package id %v does not exist", id)
					}
					imps[path] = packageInfo{string(fromPkg.Name), false}
				}
			}
		}
		return imps, nil
	}

	// Collect all the imports from the x.go, keep track of the local package name.
	if fileImports, err = collectImports(pgf.File); err != nil {
		return nil, err
	}

	testBase := strings.TrimSuffix(filepath.Base(loc.URI.Path()), ".go") + "_test.go"
	goTestFileURI := protocol.URIFromPath(filepath.Join(loc.URI.Dir().Path(), testBase))

	testFH, err := snapshot.ReadFile(ctx, goTestFileURI)
	if err != nil {
		return nil, err
	}

	// TODO(hxjiang): use a fresh name if the same test function name already
	// exist.

	var (
		eofRange protocol.Range // empty selection at end of new file
		// edits contains all the text edits to be applied to the test file.
		edits []protocol.TextEdit
		// xtest indicates whether the test file use package x or x_test.
		// TODO(hxjiang): For now, we try to interpret the user's intention by
		// reading the foo_test.go's package name. Instead, we can discuss the option
		// to interpret the user's intention by which function they are selecting.
		// Have one file for x_test package testing, one file for x package testing.
		xtest = true
	)

	testPGF, err := snapshot.ParseGo(ctx, testFH, parsego.Header)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		changes = append(changes, protocol.DocumentChangeCreate(goTestFileURI))

		// header is the buffer containing the text to add to the beginning of the file.
		var header bytes.Buffer

		// If this test file was created by the gopls, add a copyright header and
		// package decl based on the originating file.
		// Search for something that looks like a copyright header, to replicate
		// in the new file.
		if groups := pgf.File.Comments; len(groups) > 0 {
			// Copyright should appear before package decl and must be the first
			// comment group.
			// Avoid copying any other comment like package doc or directive comment.
			if c := groups[0]; c.Pos() < pgf.File.Package && c != pgf.File.Doc &&
				!isDirective(c.List[0].Text) &&
				strings.Contains(strings.ToLower(c.List[0].Text), "copyright") {
				start, end, err := pgf.NodeOffsets(c)
				if err != nil {
					return nil, err
				}
				header.Write(pgf.Src[start:end])
				// One empty line between copyright header and package decl.
				header.WriteString("\n\n")
			}
		}
		fmt.Fprintf(&header, "package %s_test\n", pkg.Types().Name())

		// Write the copyright and package decl to the beginning of the file.
		edits = append(edits, protocol.TextEdit{
			Range:   protocol.Range{},
			NewText: header.String(),
		})
	} else { // existing _test.go file.
		if testPGF.File.Name == nil || testPGF.File.Name.NamePos == token.NoPos {
			return nil, fmt.Errorf("missing package declaration")
		}
		switch testPGF.File.Name.Name {
		case pgf.File.Name.Name:
			xtest = false
		case pgf.File.Name.Name + "_test":
			xtest = true
		default:
			return nil, fmt.Errorf("invalid package declaration %q in test file %q", testPGF.File.Name, testPGF)
		}

		eofRange, err = testPGF.PosRange(testPGF.File.FileEnd, testPGF.File.FileEnd)
		if err != nil {
			return nil, err
		}

		// Collect all the imports from the foo_test.go.
		if testImports, err = collectImports(testPGF.File); err != nil {
			return nil, err
		}
	}

	// qf qualifier determines the correct package name to use for a type in
	// foo_test.go. It does this by:
	// - Consult imports map from test file foo_test.go.
	// - If not found, consult imports map from original file foo.go.
	// If the package is not imported in test file foo_test.go, it is added to
	// extraImports map.
	qf := func(p *types.Package) string {
		// When generating test in x packages, any type/function defined in the same
		// x package can emit package name.
		if !xtest && p == pkg.Types() {
			return ""
		}
		// Prefer using the package name if already defined in foo_test.go
		if local, ok := testImports[p.Path()]; ok {
			return local.name
		}
		// TODO(hxjiang): we should consult the scope of the test package to
		// ensure these new imports do not shadow any package-level names.
		// If not already imported by foo_test.go, consult the foo.go import map.
		if local, ok := fileImports[p.Path()]; ok {
			// The package that contains this type need to be added to the import
			// list in foo_test.go.
			extraImports[p.Path()] = local
			return local.name
		}
		extraImports[p.Path()] = packageInfo{name: p.Name()}
		return p.Name()
	}

	start, end, err := pgf.RangePos(loc.Range)
	if err != nil {
		return nil, err
	}

	path, _ := astutil.PathEnclosingInterval(pgf.File, start, end)
	if len(path) < 2 {
		return nil, fmt.Errorf("no enclosing function")
	}

	decl, ok := path[len(path)-2].(*ast.FuncDecl)
	if !ok {
		return nil, fmt.Errorf("no enclosing function")
	}

	fn := pkg.TypesInfo().Defs[decl.Name].(*types.Func)
	sig := fn.Signature()

	if xtest {
		// Reject if function/method is unexported.
		if !fn.Exported() {
			return nil, fmt.Errorf("cannot add test of unexported function %s to external test package %s_test", decl.Name, pgf.File.Name)
		}

		// Reject if receiver is unexported.
		if sig.Recv() != nil {
			if _, ident, _ := goplsastutil.UnpackRecv(decl.Recv.List[0].Type); !ident.IsExported() {
				return nil, fmt.Errorf("cannot add external test for method %s.%s as receiver type is not exported", ident.Name, decl.Name)
			}
		}
		// TODO(hxjiang): reject if the any input parameter type is unexported.
		// TODO(hxjiang): reject if any return value type is unexported. Explore
		// the option to drop the return value if the type is unexported.
	}

	testName, err := testName(fn)
	if err != nil {
		return nil, err
	}

	data := testInfo{
		TestingPackageName: qf(types.NewPackage("testing", "testing")),
		PackageName:        qf(pkg.Types()),
		TestFuncName:       testName,
		Func: function{
			Name: fn.Name(),
		},
	}

	errorType := types.Universe.Lookup("error").Type()

	var isContextType = func(t types.Type) bool {
		named, ok := t.(*types.Named)
		if !ok {
			return false
		}
		return named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
	}

	for i := range sig.Params().Len() {
		param := sig.Params().At(i)
		name, typ := param.Name(), param.Type()
		f := field{Type: types.TypeString(typ, qf)}
		if i == 0 && isContextType(typ) {
			f.Value = qf(types.NewPackage("context", "context")) + ".Background()"
		} else if name == "" || name == "_" {
			f.Value = typesinternal.ZeroString(typ, qf)
		} else {
			f.Name = name
		}
		data.Func.Args = append(data.Func.Args, f)
	}

	for i := range sig.Results().Len() {
		typ := sig.Results().At(i).Type()
		var name string
		if i == sig.Results().Len()-1 && types.Identical(typ, errorType) {
			name = "gotErr"
		} else if i == 0 {
			name = "got"
		} else {
			name = fmt.Sprintf("got%d", i+1)
		}
		data.Func.Results = append(data.Func.Results, field{
			Name: name,
			Type: types.TypeString(typ, qf),
		})
	}

	if sig.Recv() != nil {
		// Find the preferred type for the receiver. We don't use
		// typesinternal.ReceiverNamed here as we want to preserve aliases.
		recvType := sig.Recv().Type()
		if ptr, ok := recvType.(*types.Pointer); ok {
			recvType = ptr.Elem()
		}

		t, ok := recvType.(typesinternal.NamedOrAlias)
		if !ok {
			return nil, fmt.Errorf("the receiver type is neither named type nor alias type")
		}

		var varName string
		{
			var possibleNames []string // list of candidates, preferring earlier entries.
			if len(sig.Recv().Name()) > 0 {
				possibleNames = append(possibleNames,
					sig.Recv().Name(),            // receiver name.
					string(sig.Recv().Name()[0]), // first character of receiver name.
				)
			}
			possibleNames = append(possibleNames,
				string(t.Obj().Name()[0]), // first character of receiver type name.
			)
			if len(t.Obj().Name()) >= 2 {
				possibleNames = append(possibleNames,
					string(t.Obj().Name()[:2]), // first two character of receiver type name.
				)
			}
			var camelCase []rune
			for i, s := range t.Obj().Name() {
				if i == 0 || unicode.IsUpper(s) {
					camelCase = append(camelCase, s)
				}
			}
			possibleNames = append(possibleNames,
				string(camelCase), // captalized initials.
			)
			for _, name := range possibleNames {
				name = strings.ToLower(name)
				if name == "" || name == "t" || name == "tt" {
					continue
				}
				varName = name
				break
			}
			if varName == "" {
				varName = "r" // default as "r" for "receiver".
			}
		}

		data.Receiver = &receiver{
			Var: field{
				Name: varName,
				Type: types.TypeString(recvType, qf),
			},
		}

		// constructor is the selected constructor for type T.
		var constructor *types.Func

		// When finding the qualified constructor, the function should return the
		// any type whose named type is the same type as T's named type.
		_, wantType := typesinternal.ReceiverNamed(sig.Recv())
		for _, name := range pkg.Types().Scope().Names() {
			f, ok := pkg.Types().Scope().Lookup(name).(*types.Func)
			if !ok {
				continue
			}
			if f.Signature().Recv() != nil {
				continue
			}
			// Unexported constructor is not visible in x_test package.
			if xtest && !f.Exported() {
				continue
			}
			// Only allow constructors returning T, T, (T, error), or (T, error).
			if f.Signature().Results().Len() > 2 || f.Signature().Results().Len() == 0 {
				continue
			}

			_, gotType := typesinternal.ReceiverNamed(f.Signature().Results().At(0))
			if gotType == nil || !types.Identical(gotType, wantType) {
				continue
			}

			if f.Signature().Results().Len() == 2 && !types.Identical(f.Signature().Results().At(1).Type(), errorType) {
				continue
			}

			if constructor == nil {
				constructor = f
			}

			// Functions named NewType are prioritized as constructors over other
			// functions that match only the signature criteria.
			if strings.EqualFold(strings.ToLower(f.Name()), strings.ToLower("new"+t.Obj().Name())) {
				constructor = f
			}
		}

		if constructor != nil {
			data.Receiver.Constructor = &function{Name: constructor.Name()}
			for i := range constructor.Signature().Params().Len() {
				param := constructor.Signature().Params().At(i)
				name, typ := param.Name(), param.Type()
				f := field{Type: types.TypeString(typ, qf)}
				if i == 0 && isContextType(typ) {
					f.Value = qf(types.NewPackage("context", "context")) + ".Background()"
				} else if name == "" || name == "_" {
					f.Value = typesinternal.ZeroString(typ, qf)
				} else {
					f.Name = name
				}
				data.Receiver.Constructor.Args = append(data.Receiver.Constructor.Args, f)
			}
			for i := range constructor.Signature().Results().Len() {
				typ := constructor.Signature().Results().At(i).Type()
				var name string
				if i == 0 {
					// The first return value must be of type T, *T, or a type whose named
					// type is the same as named type of T.
					name = varName
				} else if i == constructor.Signature().Results().Len()-1 && types.Identical(typ, errorType) {
					name = "err"
				} else {
					// Drop any return values beyond the first and the last.
					// e.g., "f, _, _, err := NewFoo()".
					name = "_"
				}
				data.Receiver.Constructor.Results = append(data.Receiver.Constructor.Results, field{
					Name: name,
					Type: types.TypeString(typ, qf),
				})
			}
		}
	}

	// Resolves duplicate parameter names between the function and its
	// receiver's constructor. It adds prefix to the constructor's parameters
	// until no conflicts remain.
	if data.Receiver != nil && data.Receiver.Constructor != nil {
		seen := map[string]bool{}
		for _, f := range data.Func.Args {
			if f.Name == "" {
				continue
			}
			seen[f.Name] = true
		}

		// "" for no change, "c" for constructor, "i" for input.
		for _, prefix := range []string{"", "c", "c_", "i", "i_"} {
			conflict := false
			for _, f := range data.Receiver.Constructor.Args {
				if f.Name == "" {
					continue
				}
				if seen[prefix+f.Name] {
					conflict = true
					break
				}
			}
			if !conflict {
				for i, f := range data.Receiver.Constructor.Args {
					if f.Name == "" {
						continue
					}
					data.Receiver.Constructor.Args[i].Name = prefix + data.Receiver.Constructor.Args[i].Name
				}
				break
			}
		}
	}

	// Compute edits to update imports.
	//
	// If we're adding to an existing test file, we need to adjust existing
	// imports. Otherwise, we can simply write out the imports to the new file.
	if testPGF != nil {
		var importFixes []*imports.ImportFix
		for path, info := range extraImports {
			name := ""
			if info.renamed {
				name = info.name
			}
			importFixes = append(importFixes, &imports.ImportFix{
				StmtInfo: imports.ImportInfo{
					ImportPath: path,
					Name:       name,
				},
				FixType: imports.AddImport,
			})
		}
		importEdits, err := ComputeImportFixEdits(snapshot.Options().Local, testPGF.Src, importFixes...)
		if err != nil {
			return nil, fmt.Errorf("could not compute the import fix edits: %w", err)
		}
		edits = append(edits, importEdits...)
	} else {
		var importsBuffer bytes.Buffer
		if len(extraImports) == 1 {
			importsBuffer.WriteString("\nimport ")
			for path, info := range extraImports {
				if info.renamed {
					importsBuffer.WriteString(info.name + " ")
				}
				importsBuffer.WriteString(fmt.Sprintf("\"%s\"\n", path))
			}
		} else {
			importsBuffer.WriteString("\nimport(")
			// Loop over the map in sorted order ensures deterministic outcome.
			paths := make([]string, 0, len(extraImports))
			for key := range extraImports {
				paths = append(paths, key)
			}
			sort.Strings(paths)
			for _, path := range paths {
				importsBuffer.WriteString("\n\t")
				if extraImports[path].renamed {
					importsBuffer.WriteString(extraImports[path].name + " ")
				}
				importsBuffer.WriteString(fmt.Sprintf("\"%s\"", path))
			}
			importsBuffer.WriteString("\n)\n")
		}
		edits = append(edits, protocol.TextEdit{
			Range:   protocol.Range{},
			NewText: importsBuffer.String(),
		})
	}

	var test bytes.Buffer
	if err := testTmpl.Execute(&test, data); err != nil {
		return nil, err
	}

	edits = append(edits,
		protocol.TextEdit{
			Range:   eofRange,
			NewText: test.String(),
		})

	return append(changes, protocol.DocumentChangeEdit(testFH, edits)), nil
}

// testName returns the name of the function to use for the new function that
// tests fn.
// Returns empty string if the fn is ill typed or nil.
func testName(fn *types.Func) (string, error) {
	if fn == nil {
		return "", fmt.Errorf("input nil function")
	}
	testName := "Test"
	if recv := fn.Signature().Recv(); recv != nil { // method declaration.
		// Retrieve the unpointered receiver type to ensure the test name is based
		// on the topmost alias or named type, not the alias' RHS type (potentially
		// unexported) type.
		// For example:
		// type Foo = foo // Foo is an exported alias for the unexported type foo
		recvType := recv.Type()
		if ptr, ok := recv.Type().(*types.Pointer); ok {
			recvType = ptr.Elem()
		}

		t, ok := recvType.(typesinternal.NamedOrAlias)
		if !ok {
			return "", fmt.Errorf("receiver type is not named type or alias type")
		}

		if !t.Obj().Exported() {
			testName += "_"
		}

		testName += t.Obj().Name() + "_"
	} else if !fn.Exported() { // unexported function declaration.
		testName += "_"
	}
	return testName + fn.Name(), nil
}
