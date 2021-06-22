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
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"golang.org/x/tools/go/ast/astutil"
)

var (
	pathFlag         = flag.String("p", "", "Path to a folder that contains the benchmark code (can be in any sub folder).")
	nameFlag         = flag.String("n", "", "Regexp that matches the name of the Benchmark* function. Needs to match exactly one function.")
	noSrcCleanupFlag = flag.Bool("no-src-cleanup", false, "If true, do not clean up the temporary source directory.")
	binaryPathFlag   = flag.String("o", "", "Path of the resulting binary.")
)

func die(f string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}

func dieUsage(f string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	flag.Usage()
	os.Exit(1)
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		die("Could not find current working directory: %s", err)
	}

	flag.Parse()

	if *pathFlag == "" {
		dieUsage("Missing -p flag.")
	}

	if *nameFlag == "" {
		dieUsage("Missing -n flag.")
	}

	binaryPath := path.Join(cwd, "benchmark.binary")
	if *binaryPathFlag != "" {
		binaryPath = *binaryPathFlag
		if !path.IsAbs(binaryPath) {
			binaryPath = path.Join(cwd, binaryPath)
		}
	}

	module := *pathFlag
	nameRegex := regexp.MustCompile(".*" + *nameFlag + ".*")

	buildCtx := build.Default

	pkg, err := buildCtx.Import(module, cwd, 0)
	if err != nil {
		die("Could not import provided module '%s': %s", module, err)
	}

	foundBenchFuncs := findBenchmarkFuncs(pkg, nameRegex)
	if len(foundBenchFuncs) == 0 {
		die("Could not find any benchmark function in %s matching %s", module, nameRegex)
	}

	for _, x := range foundBenchFuncs {
		fmt.Printf("Found matching function: %s (%s)\n", x.name, x.file)
	}

	if len(foundBenchFuncs) > 1 {
		die("There should be only one matching function in %s for %s, but found %d", module, nameRegex, len(foundBenchFuncs))
	}

	benchFuncLoc := foundBenchFuncs[0]

	tmpDir, err := os.MkdirTemp("", "go-bb-*")
	if err != nil {
		die("Could not create temporary source directory: %s", err)
	}

	if !*noSrcCleanupFlag {
		defer os.Remove(tmpDir)
	}

	fmt.Println("Temporary source directory:", tmpDir)

	bborigPath := path.Join(tmpDir, "bborig")

	err = os.Mkdir(bborigPath, 0700)
	if err != nil {
		die("Could not create original source directory at '%s': %s", bborigPath, err)
	}

	err = copyModuleToTmp(pkg.Dir, bborigPath)
	if err != nil {
		die("Failed to copy original sources from '%s' to '%s': %s", pkg.Dir, bborigPath, err)
	}

	bborigModulePath, err := filepath.Rel(cwd, bborigPath)
	if err != nil {
		die("Could not compute relative path from %s to %s", cwd, bborigPath)
	}

	// bborigModule, err := buildCtx.Import(bborigModulePath, cwd, 0)
	// if err != nil {
	// 	die("Copied module is invalid: %s", err)
	// }

	fmt.Println("Rewriting benchmark function")
	err = rewriteBenchFuncInPlace(bborigModulePath, benchFuncLoc)
	if err != nil {
		die("Could not rewrite benchmark function: %s", err)
	}

	fmt.Println("Renaming test files")
	err = renameTestFiles(bborigModulePath)
	if err != nil {
		die("Could not rename test files: %s", err)
	}

	tmpModuleName := path.Base(tmpDir)
	fullTmpModule := "example.com/" + tmpModuleName

	data := templateContext{
		OrigImport: fullTmpModule + "/bborig",
		Func:       benchFuncLoc.name,
	}

	mainFilePath := path.Join(tmpDir, "main.go")
	renderMainToFile(data, mainFilePath)

	fmt.Println("Initializing module", fullTmpModule)
	err = runGo(tmpDir, "mod", "init", fullTmpModule)
	if err != nil {
		die("Failed to init module: %s", err)
	}

	fmt.Println("Running tidy")
	err = runGo(tmpDir, "mod", "tidy")
	if err != nil {
		die("Failed to tidy module: %s", err)
	}

	fmt.Println("Compiling")
	err = runGo(tmpDir, "build", "-o", binaryPath)
	if err != nil {
		die("Failed to compile benchmark binary: %s", err)
	}

	fmt.Println("Benchmark binary ready at", binaryPath)
}

func renameTestFiles(p string) error {
	files, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	for _, x := range files {
		if x.IsDir() || !strings.HasSuffix(x.Name(), "_test.go") {
			continue
		}

		newName := strings.Replace(x.Name(), "_test.go", "_bborig.go", 1)
		fromFilePath := path.Join(p, x.Name())
		toFilePath := path.Join(p, newName)
		err = os.Rename(fromFilePath, toFilePath)
		if err != nil {
			return fmt.Errorf("renaming %s to %s: %w", fromFilePath, toFilePath, err)
		}
	}
	return nil
}

func runGo(dir string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func renderMainToFile(data templateContext, filePath string) {
	out, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		die("Could not open file %s for writing: %s", filePath, out)
	}
	defer out.Close()

	t := template.Must(template.New("main").Parse(mainTemplate))
	t.Execute(out, data)
}

type templateContext struct {
	OrigImport string
	Func       string
}

const mainTemplate = `
package main

import orig "{{.OrigImport}}"

func main() {
	orig.{{.Func}}()
}
`

// 1. Find the function from loc at pkg.
// 2. Rewrite it to remove the testing.B dependency.
// 3. Overwrite the source file on disk.
func rewriteBenchFuncInPlace(pkgDir string, loc fnLoc) error {
	filePath := path.Join(pkgDir, loc.file)

	fset := token.NewFileSet()
	fileAst, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		return err
	}

	var d *ast.FuncDecl

	for _, decl := range fileAst.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Name.Name == loc.name {
			d = fd
			break
		}
	}

	if d == nil {
		panic("could not find benchmark declaration after the files have been copied")
	}

	if d.Type.Params.NumFields() != 1 {
		die("Function %s is expected to have exactly one parameter, but got %d", loc.name, d.Type.Params.NumFields())
	}

	testingBIdent := d.Type.Params.List[0].Names[0]

	// Remove all parameters
	// TODO: remove 'testing' import if it was the only reference in the file
	d.Type.Params.List = nil

	d.Body = removeReferencesToIdentifier(fset, testingBIdent, d.Body).(*ast.BlockStmt)

	// Add go:noinline comment
	if d.Doc == nil {
		d.Doc = &ast.CommentGroup{}
	}
	d.Doc.List = append(d.Doc.List, &ast.Comment{
		Text: "//go:noinline",
	})

	// Write out modified file
	out, err := os.OpenFile(filePath, os.O_RDWR|os.O_TRUNC, 0755)
	if err != nil {
		die("Could not open file %s for writing: %s", filePath, out)
	}
	defer out.Close()
	err = format.Node(out, fset, fileAst)
	if err != nil {
		die("Could not format modified source: %s", err)
	}

	return nil
}

func printNodeCode(fset *token.FileSet, node ast.Node) {
	if node == nil {
		return
	}
	var buf bytes.Buffer
	err := format.Node(&buf, fset, node)
	if err != nil {
		log.Println("warning: printNodeCode:", err)
	}

	fmt.Println(buf.String())
}

// Very not complete, also probably not sound either.
//
// - Removes calls of the form b.X(?)
// - Hoist body of for statement of the form for ?; ? < b; ? {}
//
// TODO: do all of this better. It's also where the main complexity of this problem lies.
func removeReferencesToIdentifier(fset *token.FileSet, id *ast.Ident, root ast.Node) ast.Node {
	depth := 0
	deleteMe := false

	return astutil.Apply(root, func(c *astutil.Cursor) bool {
		node := c.Node()

		// fmt.Println("---------------------------------------------")
		// fmt.Println("----[", c.Name())
		// fmt.Println("[[[[[", depth)
		// fmt.Printf("%T, %+v\n", node, node)
		// printNodeCode(fset, node)
		// fmt.Println("---------------------------------------------")

		switch v := node.(type) {
		case *ast.CallExpr:
			f := v.Fun
			sel, ok := f.(*ast.SelectorExpr)
			if ok {
				expr := sel.X
				ident, ok := expr.(*ast.Ident)
				if ok && ident.Obj == id.Obj {
					deleteMe = true
					return false
				}
			}
		case *ast.ForStmt:
			cond := v.Cond
			op, ok := cond.(*ast.BinaryExpr)
			if ok && op.Op == token.LSS {
				sel, ok := op.Y.(*ast.SelectorExpr)
				if ok {
					expr := sel.X
					ident, ok := expr.(*ast.Ident)
					if ok && ident.Obj == id.Obj {
						c.Replace(v.Body)
						break
					}
				}
			}
		}

		depth++
		return true
	}, func(c *astutil.Cursor) bool {
		depth--
		if deleteMe && c.Index() >= 0 {
			c.Delete()
			deleteMe = false
			return true
		}
		return true
	})
}

func copyModuleToTmp(fromPath, toPath string) error {
	fmt.Println("Copying from", fromPath, "->", toPath)
	files, err := os.ReadDir(fromPath)
	if err != nil {
		return err
	}

	for _, x := range files {
		if x.IsDir() || !strings.HasSuffix(x.Name(), ".go") {
			continue
		}
		fromFilePath := path.Join(fromPath, x.Name())
		toFilePath := path.Join(toPath, x.Name())
		err = copyFile(fromFilePath, toFilePath)
		if err != nil {
			return fmt.Errorf("error copying %s to %s: %w", fromFilePath, toFilePath, err)
		}
		fmt.Println("Copied", fromFilePath, "->", toFilePath)
	}
	return nil
}

func copyFile(fromPath, toPath string) error {
	fromFile, err := os.Open(fromPath)
	if err != nil {
		return err
	}
	defer fromFile.Close()

	toFile, err := os.Create(toPath)
	if err != nil {
		return err
	}
	defer toFile.Close()

	_, err = io.Copy(toFile, fromFile)
	return err
}

func findBenchmarkFuncs(pkg *build.Package, nameRegex *regexp.Regexp) []fnLoc {
	results := []fnLoc{}

	allTestFiles := make([]string, 0, len(pkg.TestGoFiles)+len(pkg.XTestGoFiles))
	allTestFiles = append(allTestFiles, pkg.TestGoFiles...)
	allTestFiles = append(allTestFiles, pkg.XTestGoFiles...)

	for _, name := range allTestFiles {
		fset := token.NewFileSet()
		p := path.Join(pkg.Dir, name)
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			fmt.Printf("%s: ignored file because it could not be parsed: %s\n", p, err)
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fd.Name.Name, "Benchmark") || !nameRegex.MatchString(fd.Name.Name) {
				continue
			}
			results = append(results, fnLoc{
				file: name,
				name: fd.Name.Name,
			})
		}
	}

	return results
}

type fnLoc struct {
	file string
	name string
}
