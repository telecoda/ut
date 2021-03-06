package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// blockVisitor walks the AST and extracts the first Block Statement it finds.
// We only use it when we've generated the code ourselves so we know there is only
// one code block to look for
type blockVisitor struct {
	stmts []ast.Stmt
}

func (v *blockVisitor) Visit(n ast.Node) ast.Visitor {
	switch n := n.(type) {
	case *ast.BlockStmt:
		v.stmts = n.List
		return nil
	}
	return v
}

// findUsedImports is an AST Visitor that notes which imports the code is using.
type findUsedImports struct {
	names map[string]struct{}
}

func newFindUsedImports() *findUsedImports {
	return &findUsedImports{make(map[string]struct{})}
}

func (v *findUsedImports) Visit(n ast.Node) ast.Visitor {
	sel, ok := n.(*ast.SelectorExpr)
	if ok {
		id, ok := sel.X.(*ast.Ident)
		if ok {
			v.names[id.Name] = struct{}{}
		}
	}
	return v
}

// isUsed indicates whether an import is used.
//
// Import specs can either just be a path, in which case the last
// path component is the name, so it can also have a separate name
func (v *findUsedImports) isUsed(s *ast.ImportSpec) bool {
	if s.Name != nil {
		_, ok := v.names[s.Name.Name]
		return ok
	}

	path := s.Path.Value
	if path[0] == '"' {
		path = path[1:]
	}
	if path[len(path)-1] == '"' {
		path = path[:len(path)-1]
	}
	parts := strings.Split(path, "/")

	name := parts[len(parts)-1]
	_, ok := v.names[name]
	return ok
}

// InterfaceVisitor walks the AST and finds interfaces.
// It also stores the imports imported by the AST
type InterfaceVisitor struct {
	name          string
	interfaceType *ast.InterfaceType
	imports       []*ast.ImportSpec
}

func (i *InterfaceVisitor) Visit(n ast.Node) ast.Visitor {
	switch n := n.(type) {
	case *ast.TypeSpec:
		t, ok := n.Type.(*ast.InterfaceType)
		if ok {
			// This is an interface
			if n.Name.Name == i.name {
				i.interfaceType = t
			}
			return nil
		}
	case *ast.ImportSpec:
		i.imports = append(i.imports, n)
	}

	return i
}

func sameDir(d1, d2 string) bool {
	a1, _ := filepath.Abs(d1)
	a2, _ := filepath.Abs(d2)
	return filepath.Clean(a1) == filepath.Clean(a2)
}

func buildMockForInterface(o *options, t *ast.InterfaceType, imports []*ast.ImportSpec) string {
	// TODO: if we're not building this mock in the package it came from then
	// we need to qualify any local types and add an import.
	// We make up a package name that's unlikely to be used

	if o.pkg != nil {
		thisdir, _ := os.Getwd()
		if !sameDir(thisdir, o.pkg.Dir) {
			if qualifyLocalTypes(t, "utmocklocal") {
				imports = append(imports, &ast.ImportSpec{
					Name: ast.NewIdent("utmocklocal"),
					Path: &ast.BasicLit{
						Kind:  token.STRING,
						Value: "\"" + o.pkg.ImportPath + "\"",
					},
				})
			}
		}
	}

	// Mock Implementation of the interface
	mockAst, fset, err := buildBasicFile(o.targetPackage, o.mockName)
	if err != nil {
		fmt.Printf("Failed to parse basic AST. %v", err)
		os.Exit(2)
	}

	// Build a map to keep track of where the comments are
	cmap := ast.NewCommentMap(fset, mockAst, mockAst.Comments)

	// Method receiver for our mock interface
	recv := buildMethodReceiver(o.mockName)

	// Add methods to our mockAst for each interface method
	for _, m := range t.Methods.List {
		t, ok := m.Type.(*ast.FuncType)
		if ok {
			// Names for return values causes problems, so remove them.
			if t.Results != nil {
				removeFieldNames(t.Results)
			}

			// We can have multiple names for a method type if multiple
			// methods are declared with the same signature
			for _, n := range m.Names {
				fd := buildMockMethod(recv, n.Name, t)

				mockAst.Decls = append(mockAst.Decls, fd)
			}
		}
	}

	addImportsToMock(mockAst, fset, imports)

	// Fixup the comments
	mockAst.Comments = cmap.Filter(mockAst).Comments()

	var buf bytes.Buffer
	format.Node(&buf, fset, mockAst)

	return buf.String()
}

func addImportsToMock(mockAst *ast.File, fset *token.FileSet, imports []*ast.ImportSpec) {
	// Find all the imports we're using in the mockAST
	fi := newFindUsedImports()
	ast.Walk(fi, mockAst)

	// Pick imports out of our input AST that are used in the mock
	usedImports := []ast.Spec{}
	for _, is := range imports {
		if fi.isUsed(is) {
			usedImports = append(usedImports, is)
		}
	}

	if len(usedImports) > 0 {
		// Add these imports into the mock AST
		ai := &addImports{usedImports}
		ast.Walk(ai, mockAst)

		// Sort the imports
		ast.SortImports(fset, mockAst)
	}
}

// removeFieldNames removes names from the FieldList in place.
// This is used to remove names from return values
func removeFieldNames(fl *ast.FieldList) {
	l := []*ast.Field{}
	for _, f := range fl.List {
		if f.Names == nil {
			l = append(l, f)
		} else {
			for range f.Names {
				nf := *f
				nf.Names = nil
				l = append(l, &nf)
			}
		}
	}
	fl.List = l
}

func buildBasicFile(packageName, mockName string) (*ast.File, *token.FileSet, error) {
	code := fmt.Sprintf(
		`
package %s

// THIS CODE IS AUTO-GENERATED BY genmock
// github.com/philpearl/ut/genmock

import (
	"testing"
	"github.com/philpearl/ut"
)

type %s struct {
	ut.CallTracker
}

func New%s(t *testing.T) *%s {
	return &%s{ut.NewCallRecords(t)}
}

func (m *%s) AddCall(name string, params ...interface{}) ut.CallTracker {
	m.CallTracker.AddCall(name, params...)
	return m
}

func (m *%s) SetReturns(params ...interface{}) ut.CallTracker {
	m.CallTracker.SetReturns(params...)
	return m
}
`, packageName, mockName, mockName, mockName, mockName, mockName, mockName)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dummy.go", code, parser.ParseComments)
	return file, fset, err
}

// Build method receiver builds a little bit of AST for the method receiver
// part of a method call
func buildMethodReceiver(name string) *ast.FieldList {
	return &ast.FieldList{
		List: []*ast.Field{
			{
				Names: []*ast.Ident{
					ast.NewIdent("i"),
				},
				Type: &ast.StarExpr{
					X: ast.NewIdent(name),
				},
			},
		},
	}
}

/* buildMockMethod builds the AST for the mock method.
The function body needs to look something like:

	r := ut.TrackCall("method", param1, param2)
	return r[0].(int), r[1].(thing)

... except we need to worry about types a little more for the
return values.  So instead we do

	r := ut.TrackCall("method", param1, param2)
	var r_0 int
	var r_1 thing
	if r[0] != nil { r_0 = r[0].(int) }
	if r[1] != nil { r_1 = r[1].(thing) }
	return r_0, r_1

... and we might have an ellipsis parameter so in fact we do

	ut__params := make([]interface{}, 2)
	ut__params[0] = param1
	ut__params[1] = param2
	r := ut.TrackCall("method", ut__params...)
	var r_0 int
	var r_1 thing
	if r[0] != nil { r_0 = r[0].(int) }
	if r[1] != nil { r_1 = r[1].(thing) }
	return r_0, r_1
*/
func buildMockMethod(recv *ast.FieldList, name string, t *ast.FuncType) *ast.FuncDecl {

	stmts := []ast.Stmt{}
	p, ellipsis, err := storeParams(t.Params)
	if err != nil {
		fmt.Printf("Failed to set up call parameters. %v", err)
	}
	if p != nil {
		stmts = append(stmts, p...)
	}

	p, err = trackCall(t.Results.NumFields(), name, ellipsis, t.Params)
	if err != nil {
		fmt.Printf("failed to track call. %v", err)
	}
	stmts = append(stmts, p...)

	p, err = declReturnValues(t.Results)
	if err != nil {
		fmt.Printf("failed to declare return values. %v", err)
	}
	stmts = append(stmts, p...)

	p, err = buildReturnStatement(t.Results.NumFields())
	if err != nil {
		fmt.Printf("failed to build return statement. %v", err)
	}
	if p != nil {
		stmts = append(stmts, p...)
	}

	// This is our method declaration
	return &ast.FuncDecl{
		Type: t,
		Name: ast.NewIdent(name),
		Recv: recv,
		Body: &ast.BlockStmt{
			List: stmts,
		},
	}
}

// storeParams handles parameters
//
// If the parameters include an ellipsis we need to copy parameters into
// an interface{} array as follows.
//
//  params := []interface{}{}
//  params[0] = p1
//  params[1] = p2
//  for i, p := range ellipsisParam {
//      params[2+i]	= p
//  }
//
// If not it is better to add the params to the call directly for performance
// reasons
func storeParams(params *ast.FieldList) ([]ast.Stmt, bool, error) {
	// Is there an ellipsis parameter?
	listlen := len(params.List)
	if listlen > 0 {
		last := params.List[len(params.List)-1]
		if _, ok := last.Type.(*ast.Ellipsis); ok {
			code := fmt.Sprintf("\tut__params := make([]interface{}, %d + len(%s))\n", params.NumFields()-1, last.Names[0].Name)
			i := 0
			for _, f := range params.List {
				for _, n := range f.Names {
					if _, ok := f.Type.(*ast.Ellipsis); ok {
						// Ellipsis expression
						code += fmt.Sprintf(`
    for j, p := range %s {
    	ut__params[%d+j] = p
    }
`, n.Name, i)
					} else {
						code += fmt.Sprintf("\tut__params[%d] = %s\n", i, n.Name)
					}
					i++
				}
			}

			stmts, err := parseCodeBlock(code)
			return stmts, true, err
		}
	}
	return nil, false, nil
}

// trackCall builds the ast for the call expression.
//
// The call looks like
//     r := i.TrackCall("method", params...)
//
// If there are no return values r := is omitted
func trackCall(numReturns int, methodName string, ellipsis bool, params *ast.FieldList) ([]ast.Stmt, error) {
	code := "\t"

	if numReturns != 0 {
		code += "r := "
	}
	code += fmt.Sprintf("i.TrackCall(\"%s\", ", methodName)

	if ellipsis {
		code += "ut__params...)\n"
	} else {
		names := []string{}
		for _, f := range params.List {
			for _, n := range f.Names {
				names = append(names, n.Name)
			}
		}
		code += strings.Join(names, ", ") + ")\n"
	}
	return parseCodeBlock(code)
}

// declReturnValues builds the return part of the call
//
func declReturnValues(results *ast.FieldList) ([]ast.Stmt, error) {
	if results.NumFields() == 0 {
		return nil, nil
	}
	stmts := []ast.Stmt{}
	for i, f := range results.List {
		// var r_X type
		stmts = append(stmts, &ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{
					&ast.ValueSpec{
						Names: []*ast.Ident{
							ast.NewIdent(fmt.Sprintf("r_%d", i)),
						},
						Type: f.Type,
					},
				},
			},
		})
		// if r[X] != nil {
		//     r_X = r[X].(type)
		// }
		stmts = append(stmts, &ast.IfStmt{
			Cond: &ast.BinaryExpr{
				X: &ast.IndexExpr{
					X: ast.NewIdent("r"),
					Index: &ast.BasicLit{
						Kind:  token.INT,
						Value: fmt.Sprintf("%d", i),
					},
				},
				Op: token.NEQ,
				Y:  ast.NewIdent("nil"),
			},
			Body: &ast.BlockStmt{
				List: []ast.Stmt{
					&ast.AssignStmt{
						Lhs: []ast.Expr{
							ast.NewIdent(fmt.Sprintf("r_%d", i)),
						},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{
							&ast.TypeAssertExpr{
								X: &ast.IndexExpr{
									X: ast.NewIdent("r"),
									Index: &ast.BasicLit{
										Kind:  token.INT,
										Value: fmt.Sprintf("%d", i),
									},
								},
								Type: f.Type,
							},
						},
					},
				},
			},
		})
	}

	return stmts, nil
}

// buildReturnStatement
//
// return r_0, r_1, r_2
func buildReturnStatement(count int) ([]ast.Stmt, error) {
	r := &ast.ReturnStmt{}
	for i := 0; i < count; i++ {
		r.Results = append(r.Results, ast.NewIdent(fmt.Sprintf("r_%d", i)))
	}
	return []ast.Stmt{r}, nil
}

func generateMockFromAst(o *options, node ast.Node) bool {
	// Find  our iterface and any imports in the AST
	v := &InterfaceVisitor{name: o.ifName}
	ast.Walk(v, node)

	if v.interfaceType != nil {
		// We found our interface!
		code := buildMockForInterface(o, v.interfaceType, v.imports)

		err := ioutil.WriteFile(o.outfile, []byte(code), 0666)
		if err != nil {
			fmt.Printf("Failed to open %s for writing", o.outfile)
			os.Exit(2)
		}
		return true
	}
	return false
}

func generateMock(o *options) {
	fset := token.NewFileSet()
	// package path can be a directory
	stat, err := os.Stat(o.packagePath)
	if err != nil {
		fmt.Printf("Failed to access %s. %v", o.packagePath, err)
	}
	if stat.IsDir() {
		pkgs, err := parser.ParseDir(fset, o.packagePath, func(fileinfo os.FileInfo) bool {
			return fileinfo.Name() != o.outfile
		}, 0)
		if err != nil {
			fmt.Printf("Failed to parse %s. %v", o.packagePath, err)
			os.Exit(2)
		}
		// Look for the type in each of the files in the directory
		for _, pkg := range pkgs {
			if generateMockFromAst(o, pkg) {
				return
			}
		}
	} else {
		p, err := parser.ParseFile(fset, o.packagePath, nil, 0)
		if err != nil {
			fmt.Printf("Failed to parse %s. %v", o.packagePath, err)
			os.Exit(2)
		}
		generateMockFromAst(o, p)
	}
}

type options struct {
	// Package where the interface can be found.
	// You can also specify the path to the go file containing the interface
	packagePath string
	// Name of the interface to Mock
	ifName string
	// Name of the file to create
	outfile string
	// Name of the mock to create
	mockName string
	// Name of the package the mock should be created in
	targetPackage string

	pkg *build.Package
}

func (o *options) validate() bool {
	if o.packagePath == "" {
		fmt.Printf("You must specify a filename or interface package")
		return false
	}
	if o.ifName == "" {
		fmt.Printf("You must specify an interface name")
		return false
	}
	if o.targetPackage == "" {
		fmt.Printf("You must specify a package name for the mock")
		return false
	}
	if o.outfile == "" {
		o.outfile = fmt.Sprintf("mock%s.go", strings.ToLower(o.ifName))
	}
	if o.mockName == "" {
		o.mockName = "Mock" + o.ifName
	}

	if !strings.HasSuffix(o.packagePath, ".go") {
		pkg, err := build.Import(o.packagePath, ".", 0)
		if err != nil {
			fmt.Printf("Could not access package %s, %v", o.packagePath, err)
			return false
		}
		o.packagePath = pkg.Dir
		o.pkg = pkg
	}

	return true
}

func (o *options) setup() {
	flag.StringVar(&o.packagePath, "package", "", "The package that contains the interface definition; Must be specified. You can also provide a path to a Go file containing the interface.")
	flag.StringVar(&o.ifName, "interface", "", "The interface that we should create a mock for; Must be specified.")
	flag.StringVar(&o.outfile, "outfile", "", "The file to create the mock in. By default will use mock<interface>.go in the current directory.")
	flag.StringVar(&o.mockName, "mock", "", "The name for the mock class. By default will use Mock<interface>.")
	flag.StringVar(&o.targetPackage, "mock-package", "", "Package name to use for the mock file; Must be specified.")
}

func main() {
	o := &options{}
	o.setup()

	flag.Parse()

	if !o.validate() {
		flag.Usage()
		os.Exit(2)
	}

	generateMock(o)
}
