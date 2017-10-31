package converter

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/yunabe/lgo/core" // This is also important to install core package to GOPATH when this package is tested with go test.
	"github.com/yunabe/lgo/parser"
)

const lgoInitFuncName = "lgo_init"

var defaultImporter = importer.Default()

// ErrorList is a list of *Errors.
// The zero value for an ErrorList is an empty ErrorList ready to use.
//
type ErrorList []error

// Add adds an Error with given position and error message to an ErrorList.
func (p *ErrorList) Add(err error) {
	*p = append(*p, err)
}

// An ErrorList implements the error interface.
func (p ErrorList) Error() string {
	switch len(p) {
	case 0:
		return "no errors"
	case 1:
		return p[0].Error()
	}
	return fmt.Sprintf("%s (and %d more errors)", p[0], len(p)-1)
}

func uniqueSortedNames(ids []*ast.Ident) []string {
	var s []string
	m := make(map[string]bool)
	for _, id := range ids {
		if m[id.Name] || id.Name == "_" {
			continue
		}
		m[id.Name] = true
		s = append(s, id.Name)
	}
	sort.Sort(sort.StringSlice(s))
	return s
}

func parseLesserGoString(src string) (*token.FileSet, *parser.LGOBlock, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseLesserGoFile(fset, "", []byte(src), 0)
	return fset, f, err
}

type phase1Out struct {
	vars       []*ast.Ident
	initFunc   *ast.FuncDecl
	file       *ast.File
	consumeAll *ast.AssignStmt

	// The last expression of lgo if exists. This expression will be rewritten later
	// to print the last expression.
	// If the last expression is not a function call, the expression is wrapped with panic
	// and lastExprWrapped is set to true.
	lastExpr        *ast.ExprStmt
	lastExprWrapped bool
}

func convertToPhase1(blk *parser.LGOBlock) (out phase1Out) {
	var decls []ast.Decl
	var initBody []ast.Stmt
	for _, stmt := range blk.Stmts {
		if decl, ok := stmt.(*ast.DeclStmt); ok {
			if gen, ok := decl.Decl.(*ast.GenDecl); ok {
				if gen.Tok == token.CONST || gen.Tok == token.VAR {
					initBody = append(initBody, stmt)
					if gen.Tok == token.VAR {
						for _, spec := range gen.Specs {
							spec := spec.(*ast.ValueSpec)
							for _, indent := range spec.Names {
								out.vars = append(out.vars, indent)
							}
						}
					}
					continue
				}
			}
			decls = append(decls, decl.Decl)
			continue
		}
		initBody = append(initBody, stmt)
		if assign, ok := stmt.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, l := range assign.Lhs {
				if ident, ok := l.(*ast.Ident); ok {
					out.vars = append(out.vars, ident)
				}
			}
		}
	}
	if initBody != nil {
		// Handle the last expression.
		last := initBody[len(initBody)-1]
		if es, ok := last.(*ast.ExprStmt); ok {
			out.lastExpr = es
			if _, ok := es.X.(*ast.CallExpr); !ok {
				// If the last expr is not function call, wrap it with panic to avoid "is not used" error.
				// You should not wrap function calls becuase panic(novalue()) is also invalid in Go.
				es.X = &ast.CallExpr{
					Fun:  ast.NewIdent("panic"),
					Args: []ast.Expr{es.X},
				}
				out.lastExprWrapped = true
			}
		}
	}

	if out.vars != nil {
		// Create consumeAll.
		varNames := uniqueSortedNames(out.vars)
		var lhs, rhs []ast.Expr
		for _, name := range varNames {
			lhs = append(lhs, &ast.Ident{Name: "_"})
			rhs = append(rhs, &ast.Ident{Name: name})
		}
		out.consumeAll = &ast.AssignStmt{
			Lhs: lhs,
			Rhs: rhs,
			Tok: token.ASSIGN,
		}
		initBody = append(initBody, out.consumeAll)
	}

	out.initFunc = &ast.FuncDecl{
		Name: ast.NewIdent(lgoInitFuncName),
		Type: &ast.FuncType{},
		Body: &ast.BlockStmt{
			List: initBody,
		},
	}
	decls = append(decls, out.initFunc)
	out.file = &ast.File{
		Doc:        blk.Doc,
		Package:    token.NoPos,
		Name:       ast.NewIdent("lgo_exec"), // Set a proper name.
		Decls:      decls,
		Scope:      blk.Scope,
		Imports:    blk.Imports,
		Unresolved: nil,
		Comments:   blk.Comments,
	}
	return
}

func convertToPhase2(ph1 phase1Out, pkg *types.Package, checker *types.Checker, conf *Config, runctx types.Object) {
	immg := newImportManager(pkg, ph1.file, checker)
	prependPkgToOlds(conf, checker, ph1.file, immg)
	// TODO: Should we move this after final check?
	if runctx != nil {
		rewriteExpr(ph1.file, func(expr ast.Expr) ast.Expr {
			id, ok := expr.(*ast.Ident)
			if !ok {
				return expr
			}
			if checker.Uses[id] != runctx {
				return expr
			}
			corePkg, err := defaultImporter.Import(core.SelfPkgPath)
			if err != nil {
				panic(fmt.Sprintf("Failed to import core: %v", err))
			}
			return &ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X:   &ast.Ident{Name: immg.shortName(corePkg)},
					Sel: &ast.Ident{Name: "GetExecContext"},
				},
			}
		})
	}

	var newInitBody []ast.Stmt
	var varSpecs []ast.Spec
	for _, stmt := range ph1.initFunc.Body.List {
		if stmt == ph1.consumeAll {
			continue
		}
		if stmt == ph1.lastExpr {
			var target ast.Expr
			if ph1.lastExprWrapped {
				target = ph1.lastExpr.X.(*ast.CallExpr).Args[0]
			} else if tuple, ok := checker.Types[ph1.lastExpr.X].Type.(*types.Tuple); !ok || tuple.Len() > 0 {
				// "!ok" means single return value.
				target = ph1.lastExpr.X
			}
			if target != nil {
				corePkg, err := defaultImporter.Import(core.SelfPkgPath)
				if err != nil {
					panic(fmt.Sprintf("Failed to import core: %v", err))
				}

				ph1.lastExpr.X = &ast.CallExpr{
					Fun: &ast.SelectorExpr{
						X:   &ast.Ident{Name: immg.shortName(corePkg)},
						Sel: &ast.Ident{Name: "LgoPrintln"},
					},
					Args: []ast.Expr{target},
				}
			}
		}
		if decl, ok := stmt.(*ast.DeclStmt); ok {
			gen := decl.Decl.(*ast.GenDecl)
			if gen.Tok == token.VAR {
				for _, spec := range gen.Specs {
					spec := spec.(*ast.ValueSpec)
					for _, name := range spec.Names {
						if vspec := varSpecFromIdent(immg, pkg, name, checker); vspec != nil {
							varSpecs = append(varSpecs, vspec)
						}
					}
					if spec.Values != nil {
						var lhs []ast.Expr
						for _, name := range spec.Names {
							lhs = append(lhs, &ast.Ident{Name: name.Name})
						}
						newInitBody = append(newInitBody, &ast.AssignStmt{
							Lhs: lhs,
							Rhs: spec.Values,
							Tok: token.ASSIGN,
						})
					}
				}
			} else if gen.Tok == token.CONST {
				ph1.file.Decls = append(ph1.file.Decls, gen)
			} else {
				panic(fmt.Sprintf("Unexpected token: %v", gen.Tok))
			}
			continue
		}
		newInitBody = append(newInitBody, stmt)
		if assign, ok := stmt.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			// Rewrite := with =.
			assign.Tok = token.ASSIGN
			// Define vars.
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok {
					if vspec := varSpecFromIdent(immg, pkg, ident, checker); vspec != nil {
						varSpecs = append(varSpecs, vspec)
					}
				}
			}
		}
	}
	ph1.initFunc.Body.List = newInitBody

	if varSpecs != nil {
		ph1.file.Decls = append(ph1.file.Decls, &ast.GenDecl{
			// go/printer prints multiple vars only when Lparen is set.
			Lparen: 1,
			Rparen: 2,
			Tok:    token.VAR,
			Specs:  varSpecs,
		})
	}
	if varSpecs != nil && conf.RegisterVars {
		corePkg, err := defaultImporter.Import(core.SelfPkgPath)
		if err != nil {
			panic(fmt.Sprintf("Failed to import core: %v", err))
		}
		var registers []ast.Stmt
		for _, vs := range varSpecs {
			// TODO: Reconsider varSpecs type.
			for _, name := range vs.(*ast.ValueSpec).Names {
				call := &ast.CallExpr{
					Fun: &ast.SelectorExpr{
						X:   &ast.Ident{Name: immg.shortName(corePkg)},
						Sel: &ast.Ident{Name: "LgoRegisterVar"},
					},
					Args: []ast.Expr{
						&ast.BasicLit{
							Kind:  token.STRING,
							Value: fmt.Sprintf("%q", name),
						},
						&ast.UnaryExpr{
							Op: token.AND,
							X:  ast.NewIdent(name.Name),
						},
					},
				}
				registers = append(registers, &ast.ExprStmt{X: call})
			}
		}
		ph1.initFunc.Body.List = append(registers, ph1.initFunc.Body.List...)
	}

	var newDels []ast.Decl
	for _, im := range immg.injectedImports {
		newDels = append(newDels, im)
	}
	for _, decl := range ph1.file.Decls {
		if newInitBody == nil && decl == ph1.initFunc {
			// Remove initBody if it's empty now.
			continue
		}
		newDels = append(newDels, decl)
	}
	ph1.file.Decls = newDels
}

type importManager struct {
	checker   *types.Checker
	current   *types.Package
	fileScope *types.Scope
	names     map[*types.Package]string
	counter   int

	// Outputs
	injectedImports []*ast.GenDecl
}

func newImportManager(current *types.Package, file *ast.File, checker *types.Checker) *importManager {
	fileScope := checker.Scopes[file]
	names := make(map[*types.Package]string)
	for _, name := range fileScope.Names() {
		obj := fileScope.Lookup(name)
		pname, ok := obj.(*types.PkgName)
		if ok {
			names[pname.Imported()] = name
		}
	}
	return &importManager{
		checker:   checker,
		current:   current,
		fileScope: fileScope,
		names:     names,
		counter:   0,
	}
}

func (m *importManager) shortName(pkg *types.Package) string {
	if pkg == m.current {
		return ""
	}
	n, ok := m.names[pkg]
	if ok {
		return n
	}
	for {
		n = fmt.Sprintf("pkg%d", m.counter)
		m.counter++
		if _, obj := m.fileScope.LookupParent(n, token.NoPos); obj == nil {
			break
		}
		// name conflict. Let's continue.
	}
	m.names[pkg] = n
	m.injectedImports = append(m.injectedImports, &ast.GenDecl{
		Tok: token.IMPORT,
		Specs: []ast.Spec{
			&ast.ImportSpec{
				Name: ast.NewIdent(n),
				Path: &ast.BasicLit{
					Kind:  token.STRING,
					Value: fmt.Sprintf("%q", pkg.Path()),
				},
			},
		},
	})
	return n
}

func varSpecFromIdent(immg *importManager, pkg *types.Package, ident *ast.Ident, checker *types.Checker) *ast.ValueSpec {
	obj := checker.Defs[ident]
	if obj == nil {
		return nil
	}
	if basic, ok := obj.Type().(*types.Basic); ok && basic.Kind() == types.Invalid {
		// This check is important when convertToPhase2 is called from inspectObject.
		return nil
	}
	typStr := types.TypeString(obj.Type(), func(pkg *types.Package) string {
		return immg.shortName(pkg)
	})
	typExr, err := parser.ParseExpr(typStr)
	if err != nil {
		panic(fmt.Sprintf("Failed to parse type expr %q: %v", typStr, err))
	}
	return &ast.ValueSpec{
		Names: []*ast.Ident{&ast.Ident{Name: ident.Name}},
		Type:  typExr,
	}
}

type Config struct {
	Olds         []types.Object
	OldImports   []*types.PkgName
	DefPrefix    string
	RefPrefix    string
	LgoPkgPath   string
	AutoExitCode bool
	RegisterVars bool
}

type ConvertResult struct {
	Src     []byte
	Pkg     *types.Package
	Checker *types.Checker
	Imports []*types.PkgName
	Err     error
}

// findIdentWithPos finds an ast.Ident node at pos. Returns nil if pos does not point an Ident.
func findIdentWithPos(node ast.Node, pos token.Pos) *ast.Ident {
	v := &findIdentVisitor{pos: pos}
	ast.Walk(v, node)
	return v.ident
}

type findIdentVisitor struct {
	pos   token.Pos
	ident *ast.Ident
}

func (v *findIdentVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil || v.ident != nil {
		return nil
	}
	if v.pos < node.Pos() || node.End() <= v.pos {
		return nil
	}
	if id, ok := node.(*ast.Ident); ok {
		v.ident = id
		return nil
	}
	return v
}

func InspectIdent(src string, pos token.Pos, conf *Config) (doc, query string) {
	obj, local := inspectObject(src, pos, conf)
	if obj == nil {
		return
	}
	return getDocOrGoDocQuery(obj, local)
}

func inspectObject(src string, pos token.Pos, conf *Config) (obj types.Object, isLocal bool) {
	// TODO: Consolidate code with Convert.
	fset, blk, _ := parseLesserGoString(src)
	var target *ast.Ident
	for _, stmt := range blk.Stmts {
		if id := findIdentWithPos(stmt, pos); id != nil {
			target = id
			break
		}
	}
	if target == nil {
		return nil, false
	}
	phase1 := convertToPhase1(blk)

	// TODO: Add a proper name to the package though it's not used at this moment.
	pkg, vscope := types.NewPackageWithOldValues("cmd/hello", "", conf.Olds)
	// TODO: Come up with better implementation to resolve pkg <--> vscope circular deps.
	for _, im := range conf.OldImports {
		pname := types.NewPkgName(token.NoPos, pkg, im.Name(), im.Imported())
		vscope.Insert(pname)
	}
	var runctx types.Object
	if vscope.Lookup("runctx") == nil {
		ctxP, err := defaultImporter.Import("context")
		if err != nil {
			panic(fmt.Sprintf("Failed to import context: %v", err))
		}
		runctx = types.NewVar(token.NoPos, pkg, "runctx", ctxP.Scope().Lookup("Context").Type())
		vscope.Insert(runctx)
	}

	// var errs []error
	chConf := &types.Config{
		Importer: defaultImporter,
		Error: func(err error) {
			//	errs = append(errs, err)
		},
		IgnoreFuncBodies:  true,
		DontIgnoreLgoInit: true,
	}
	var info = types.Info{
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
		Types:  make(map[ast.Expr]types.TypeAndValue),
	}
	checker := types.NewChecker(chConf, fset, pkg, &info)
	checker.Files([]*ast.File{phase1.file})

	convertToPhase2(phase1, pkg, checker, conf, runctx)
	{
		chConf := &types.Config{
			Importer: defaultImporter,
			Error: func(err error) {
				//	errs = append(errs, err)
			},
			IgnoreFuncBodies:  false,
			DontIgnoreLgoInit: true,
		}
		var info = types.Info{
			Defs:   make(map[*ast.Ident]types.Object),
			Uses:   make(map[*ast.Ident]types.Object),
			Scopes: make(map[ast.Node]*types.Scope),
			Types:  make(map[ast.Expr]types.TypeAndValue),
		}
		checker := types.NewChecker(chConf, fset, pkg, &info)
		checker.Files([]*ast.File{phase1.file})
		obj := checker.Uses[target]
		if obj == nil {
			return nil, false
		}
		return obj, obj.Pkg() == pkg
	}
}

// getDocOrGoDocQuery returns a doc string for obj or a query to retrieve a document with go doc (An argument of go doc command).
// getDocOrGoDocQuery returns ("", "") if we do not show anything for obj.
func getDocOrGoDocQuery(obj types.Object, isLocal bool) (doc string, query string) {
	if pkg, _ := obj.(*types.PkgName); pkg != nil {
		query = pkg.Imported().Path()
		return
	}
	if fn, _ := obj.(*types.Func); fn != nil {
		if isLocal {
			doc = fn.Type().String()
			return
		}
		sig := fn.Type().(*types.Signature)
		recv := sig.Recv()
		if recv == nil {
			query = fn.Pkg().Path() + "." + fn.Name()
			return
		}
		recv.Name()
		var recvName string
		switch recv := recv.Type().(type) {
		case *types.Named:
			recvName = recv.Obj().Name()
		case *types.Pointer:
			recvName = recv.Elem().(*types.Named).Obj().Name()
		case *types.Interface:
			scope := fn.Pkg().Scope()
			for _, name := range scope.Names() {
				if tyn, _ := scope.Lookup(name).(*types.TypeName); tyn != nil {
					if named, _ := tyn.Type().(*types.Named); named != nil {
						if named.Underlying() == recv {
							recvName = name
							break
						}
					}
				}
			}
		default:
			panic(fmt.Errorf("not supported: %#v", recv))
		}
		if recvName != "" {
			query = fn.Pkg().Path() + "." + recvName + "." + fn.Name()
		}
		return
	}
	if v, _ := obj.(*types.Var); v != nil {
		if v.IsField() {
			// Not implemented
			return
		}
		if isLocal {
			doc = v.String()
			return
		}
		query = v.Pkg().Path() + "." + v.Name()
		return
	}
	if c, _ := obj.(*types.Const); c != nil {
		if isLocal {
			// Not implemented
			doc = c.String()
			return
		}
		query = c.Pkg().Path() + "." + c.Name()
	}
	if tyn, _ := obj.(*types.TypeName); tyn != nil {
		if isLocal {
			panic("not implemented")
		}
		query = tyn.Pkg().Path() + "." + tyn.Name()
		return
	}
	return
}

func Convert(src string, conf *Config) *ConvertResult {
	fset, blk, err := parseLesserGoString(src)
	if err != nil {
		return &ConvertResult{Err: err}
	}
	phase1 := convertToPhase1(blk)

	// TODO: Add a proper name to the package though it's not used at this moment.
	pkg, vscope := types.NewPackageWithOldValues("cmd/hello", "", conf.Olds)
	// TODO: Come up with better implementation to resolve pkg <--> vscope circular deps.
	for _, im := range conf.OldImports {
		pname := types.NewPkgName(token.NoPos, pkg, im.Name(), im.Imported())
		vscope.Insert(pname)
	}
	var runctx types.Object
	if vscope.Lookup("runctx") == nil {
		ctxP, err := defaultImporter.Import("context")
		if err != nil {
			panic(fmt.Sprintf("Failed to import context: %v", err))
		}
		runctx = types.NewVar(token.NoPos, pkg, "runctx", ctxP.Scope().Lookup("Context").Type())
		vscope.Insert(runctx)
	}

	var errs []error
	chConf := &types.Config{
		Importer: defaultImporter,
		Error: func(err error) {
			errs = append(errs, err)
		},
		IgnoreFuncBodies:  true,
		DontIgnoreLgoInit: true,
	}
	var info = types.Info{
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
		Types:  make(map[ast.Expr]types.TypeAndValue),
	}
	checker := types.NewChecker(chConf, fset, pkg, &info)
	checker.Files([]*ast.File{phase1.file})
	if len(errs) > 0 {
		var err error
		if len(errs) > 1 {
			err = ErrorList(errs)
		} else {
			err = errs[0]
		}
		return &ConvertResult{Err: err}
	}
	convertToPhase2(phase1, pkg, checker, conf, runctx)

	fsrc, fpkg, fcheck, err := finalCheckAndRename(phase1.file, fset, conf)
	if err != nil {
		return &ConvertResult{Err: err}
	}

	var imports []*types.PkgName
	fscope := checker.Scopes[phase1.file]
	for _, name := range fscope.Names() {
		obj := fscope.Lookup(name)
		if pname, ok := obj.(*types.PkgName); ok {
			imports = append(imports, pname)
		}
	}

	return &ConvertResult{
		Src:     fsrc,
		Pkg:     fpkg,
		Checker: fcheck,
		Imports: imports,
	}
}

type importerWithOlds struct {
	olds map[string]*types.Package
}

func newImporterWithOlds(olds []types.Object) *importerWithOlds {
	m := make(map[string]*types.Package)
	for _, old := range olds {
		m[old.Pkg().Path()] = old.Pkg()
	}
	return &importerWithOlds{m}
}

func (im *importerWithOlds) Import(path string) (*types.Package, error) {
	if pkg := im.olds[path]; pkg != nil {
		return pkg, nil
	}
	return defaultImporter.Import(path)
}

// qualifiedIDFinder finds *ast.Ident that is used as "sel" of "pkg.sel".
// The output of this visitor is used not to rename "pkg.sel" to "pkg.pkg.sel".
// This logic is required for prependPkgToOlds in finalCheckAndRename.
//
// This mechnism is important because the first prependPkgToOlds (at the top of convertToPhase2) is
// also necessary to handle `x := x * x` in TestConvert_twoLgo2.
type qualifiedIDFinder struct {
	checker     *types.Checker
	qualifiedID map[*ast.Ident]bool
}

func (f *qualifiedIDFinder) Visit(node ast.Node) (w ast.Visitor) {
	sel, _ := node.(*ast.SelectorExpr)
	if sel == nil {
		return f
	}
	x, _ := sel.X.(*ast.Ident)
	if x == nil {
		return f
	}
	pname, _ := f.checker.Uses[x].(*types.PkgName)
	if pname == nil {
		return f
	}
	f.qualifiedID[sel.Sel] = true
	return f
}

func prependPkgToOlds(conf *Config, checker *types.Checker, file *ast.File, immg *importManager) {
	// Add package names to identities that refers to old values.
	isOld := make(map[types.Object]bool)
	for _, old := range conf.Olds {
		isOld[old] = true
	}
	qif := &qualifiedIDFinder{
		checker:     checker,
		qualifiedID: make(map[*ast.Ident]bool),
	}
	ast.Walk(qif, file)
	rewriteExpr(file, func(expr ast.Expr) ast.Expr {
		id, ok := expr.(*ast.Ident)
		if !ok {
			return expr
		}
		obj, ok := checker.Uses[id]
		if !ok {
			return expr
		}
		if !isOld[obj] || qif.qualifiedID[id] {
			return expr
		}
		return &ast.SelectorExpr{
			X:   &ast.Ident{Name: immg.shortName(obj.Pkg())},
			Sel: id,
		}
	})
}

// prependPrefixToID prepends a prefix to the name of ident.
// It prepends the prefix the last element if ident.Name contains "."
func prependPrefixToID(indent *ast.Ident, prefix string) {
	idx := strings.LastIndex(indent.Name, ".")
	if idx == -1 {
		indent.Name = prefix + indent.Name
	} else {
		indent.Name = indent.Name[:idx+1] + prefix + indent.Name[idx+1:]
	}
}

func finalCheckAndRename(file *ast.File, fset *token.FileSet, conf *Config) ([]byte, *types.Package, *types.Checker, error) {
	var errs []error
	chConf := &types.Config{
		Importer: newImporterWithOlds(conf.Olds),
		Error: func(err error) {
			errs = append(errs, err)
		},
		DisableUnusedImportCheck: true,
	}
	pkg, vscope := types.NewPackageWithOldValues(conf.LgoPkgPath, "", conf.Olds)
	var oldImports []*types.PkgName
	// TODO: Come up with better implementation to resolve pkg <--> vscope circular deps.
	for _, im := range conf.OldImports {
		pname := types.NewPkgName(token.NoPos, pkg, im.Name(), im.Imported())
		vscope.Insert(pname)
		oldImports = append(oldImports, pname)
	}
	pkg.IsLgo = true
	info := &types.Info{
		Defs:      make(map[*ast.Ident]types.Object),
		Uses:      make(map[*ast.Ident]types.Object),
		Scopes:    make(map[ast.Node]*types.Scope),
		Implicits: make(map[ast.Node]types.Object),
	}
	checker := types.NewChecker(chConf, fset, pkg, info)
	checker.Files([]*ast.File{file})
	if errs != nil {
		// TODO: Return all errors.
		return nil, nil, nil, errs[0]
	}

	for ident, obj := range checker.Defs {
		if ast.IsExported(ident.Name) || ident.Name == lgoInitFuncName {
			continue
		}
		if obj == nil {
			// ident is the top-level package declaration. Skip this.
			continue
		}
		scope := pkg.Scope()
		if scope != nil && scope.Lookup(obj.Name()) == obj {
			// Rename package level symbol.
			ident.Name = conf.DefPrefix + ident.Name
		} else if _, ok := obj.(*types.Func); ok {
			// Rename methods.
			// Notes: *types.Func is top-level func or methods (methods are not necessarily top-level).
			//        inlined-functions are *types.Var.
			ident.Name = conf.DefPrefix + ident.Name
		} else if v, ok := obj.(*types.Var); ok && v.IsField() {
			ident.Name = conf.DefPrefix + ident.Name
		}
	}
	immg := newImportManager(pkg, file, checker)
	prependPkgToOlds(conf, checker, file, immg)

	// Inject auto-exit code
	if conf.AutoExitCode {
		injectAutoExitToFile(file, immg)
	}
	capturePanicInGoRoutine(file, immg, checker.Defs)

	// Import lgo packages implicitly referred code inside functions.
	var newDels []ast.Decl
	for _, im := range immg.injectedImports {
		newDels = append(newDels, im)
	}
	// Import old imports.
	for _, im := range oldImports {
		if !im.Used() {
			continue
		}
		newDels = append(newDels, &ast.GenDecl{
			Tok: token.IMPORT,
			Specs: []ast.Spec{
				&ast.ImportSpec{
					Name: ast.NewIdent(im.Name()),
					Path: &ast.BasicLit{
						Kind:  token.STRING,
						Value: fmt.Sprintf("%q", im.Imported().Path()),
					},
				},
			},
		})
	}
	// Remove unused imports.
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			newDels = append(newDels, decl)
			continue
		}
		var specs []ast.Spec
		for _, spec := range gen.Specs {
			spec := spec.(*ast.ImportSpec)
			var pname *types.PkgName
			if spec.Name != nil {
				pname = checker.Defs[spec.Name].(*types.PkgName)
			} else {
				pname = checker.Implicits[spec].(*types.PkgName)
			}
			if pname == nil {
				panic(fmt.Sprintf("*types.PkgName for %v not found", spec))
			}
			if pname.Used() {
				specs = append(specs, spec)
			}
		}
		if specs != nil {
			gen.Specs = specs
			newDels = append(newDels, gen)
		}
	}
	if len(newDels) == 0 {
		// Nothing is left. Return an empty source.
		return nil, pkg, checker, nil
	}
	file.Decls = newDels
	for ident, obj := range checker.Uses {
		if ast.IsExported(ident.Name) {
			continue
		}
		pkg := obj.Pkg()
		if pkg == nil || !pkg.IsLgo {
			continue
		}
		if pkg.Scope().Lookup(obj.Name()) == obj {
			// Rename package level symbol.
			prependPrefixToID(ident, conf.RefPrefix)
		} else if _, ok := obj.(*types.Func); ok {
			// Rename methods.
			prependPrefixToID(ident, conf.RefPrefix)
		} else if v, ok := obj.(*types.Var); ok && v.IsField() {
			prependPrefixToID(ident, conf.RefPrefix)
		}
	}
	var buf bytes.Buffer
	err := format.Node(&buf, token.NewFileSet(), file)
	if err != nil {
		return nil, nil, nil, err
	}
	return buf.Bytes(), pkg, checker, nil
}

func capturePanicInGoRoutine(file *ast.File, immg *importManager, defs map[*ast.Ident]types.Object) {
	picker := newNamePicker(defs)
	ast.Walk(&wrapGoStmtVisitor{immg, picker}, file)
}

// wrapGoStmtVisitor injects code to wrap go statements.
//
// This converts
// go f(x, y)
// to
// go func() {
//   defer core.FinalizeGoRoutine(core.InitGoroutine())
//   f(x, y)
// }()
type wrapGoStmtVisitor struct {
	immg   *importManager
	picker *namePicker
}

func (v *wrapGoStmtVisitor) Visit(node ast.Node) ast.Visitor {
	b, ok := node.(*ast.BlockStmt)
	if !ok {
		return v
	}
	corePkg, _ := defaultImporter.Import(core.SelfPkgPath)
	for i, stmt := range b.List {
		ast.Walk(v, stmt)
		g, ok := stmt.(*ast.GoStmt)
		if !ok {
			continue
		}
		ectx := v.picker.NewName("ectx")
		fu := &ast.FuncLit{
			Type: &ast.FuncType{},
			Body: &ast.BlockStmt{
				List: []ast.Stmt{
					&ast.DeferStmt{
						Call: &ast.CallExpr{
							Fun: &ast.SelectorExpr{
								X:   &ast.Ident{Name: v.immg.shortName(corePkg)},
								Sel: &ast.Ident{Name: "FinalizeGoroutine"},
							},
							Args: []ast.Expr{&ast.Ident{Name: ectx}},
						},
					},
					&ast.ExprStmt{X: g.Call},
				},
			},
		}
		g.Call = &ast.CallExpr{Fun: fu}

		b.List[i] = &ast.BlockStmt{
			List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{&ast.Ident{Name: ectx}},
					Rhs: []ast.Expr{&ast.CallExpr{
						Fun: ast.NewIdent(v.immg.shortName(corePkg) + ".InitGoroutine"),
					}},
					Tok: token.DEFINE,
				},
				g,
			},
		}
	}
	// Do not visit this node again.
	return nil
}
