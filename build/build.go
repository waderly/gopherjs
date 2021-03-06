package build

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/kardianos/osext"
	"github.com/gopherjs/gopherjs/compiler"
	"github.com/neelance/sourcemap"
	"gopkg.in/fsnotify.v1"
)

type ImportCError struct{}

func (e *ImportCError) Error() string {
	return `importing "C" is not supported by GopherJS`
}

func NewBuildContext(archSuffix string) *build.Context {
	if strings.HasPrefix(runtime.Version(), "go1.") && runtime.Version()[4] < '3' {
		panic("GopherJS requires Go 1.3. Please upgrade.")
	}
	return &build.Context{
		GOROOT:      build.Default.GOROOT,
		GOPATH:      build.Default.GOPATH,
		GOOS:        build.Default.GOOS,
		GOARCH:      archSuffix,
		Compiler:    "gc",
		BuildTags:   []string{"netgo"},
		ReleaseTags: build.Default.ReleaseTags,
	}
}

func Import(path string, mode build.ImportMode, archSuffix string) (*build.Package, error) {
	if path == "C" {
		return nil, &ImportCError{}
	}

	buildContext := NewBuildContext(archSuffix)
	if path == "runtime" || path == "syscall" {
		buildContext.GOARCH = build.Default.GOARCH
		buildContext.InstallSuffix = archSuffix
	}
	pkg, err := buildContext.Import(path, "", mode)
	if path == "hash/crc32" {
		pkg.GoFiles = []string{"crc32.go", "crc32_generic.go"}
	}
	if pkg.IsCommand() {
		pkg.PkgObj = filepath.Join(pkg.BinDir, filepath.Base(pkg.ImportPath)+".js")
	}
	if _, err := os.Stat(pkg.PkgObj); os.IsNotExist(err) && strings.HasPrefix(pkg.PkgObj, build.Default.GOROOT) {
		// fall back to GOPATH
		firstGopathWorkspace := filepath.SplitList(build.Default.GOPATH)[0] // TODO: Need to check inside all GOPATH workspaces.
		gopathPkgObj := filepath.Join(firstGopathWorkspace, pkg.PkgObj[len(build.Default.GOROOT):])
		if _, err := os.Stat(gopathPkgObj); err == nil {
			pkg.PkgObj = gopathPkgObj
		}
	}
	return pkg, err
}

func Parse(pkg *build.Package, fileSet *token.FileSet) ([]*ast.File, error) {
	var files []*ast.File
	replacedDeclNames := make(map[string]bool)
	funcName := func(d *ast.FuncDecl) string {
		if d.Recv == nil {
			return d.Name.Name
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		return recv.(*ast.Ident).Name + "." + d.Name.Name
	}
	isTestPkg := strings.HasSuffix(pkg.ImportPath, "_test")
	importPath := pkg.ImportPath
	if isTestPkg {
		importPath = importPath[:len(importPath)-5]
	}
	if nativesPkg, err := Import("github.com/gopherjs/gopherjs/compiler/natives/"+importPath, 0, "js"); err == nil {
		names := append(nativesPkg.GoFiles, nativesPkg.TestGoFiles...)
		if isTestPkg {
			names = nativesPkg.XTestGoFiles
		}
		for _, name := range names {
			file, err := parser.ParseFile(fileSet, filepath.Join(nativesPkg.Dir, name), nil, parser.ParseComments)
			if err != nil {
				panic(err)
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					replacedDeclNames[funcName(d)] = true
				case *ast.GenDecl:
					switch d.Tok {
					case token.TYPE:
						for _, spec := range d.Specs {
							replacedDeclNames[spec.(*ast.TypeSpec).Name.Name] = true
						}
					case token.VAR, token.CONST:
						for _, spec := range d.Specs {
							for _, name := range spec.(*ast.ValueSpec).Names {
								replacedDeclNames[name.Name] = true
							}
						}
					}
				}
			}
			files = append(files, file)
		}
	}
	delete(replacedDeclNames, "init")

	var errList compiler.ErrorList
	for _, name := range pkg.GoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(pkg.Dir, name)
		}
		r, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fileSet, name, r, parser.ParseComments)
		r.Close()
		if err != nil {
			if list, isList := err.(scanner.ErrorList); isList {
				for _, entry := range list {
					errList = append(errList, entry)
				}
				continue
			}
			errList = append(errList, err)
			continue
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if replacedDeclNames[funcName(d)] {
					d.Name = ast.NewIdent("_")
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						s := spec.(*ast.TypeSpec)
						if replacedDeclNames[s.Name.Name] {
							s.Name = ast.NewIdent("_")
						}
					}
				case token.VAR, token.CONST:
					for _, spec := range d.Specs {
						s := spec.(*ast.ValueSpec)
						for i, name := range s.Names {
							if replacedDeclNames[name.Name] {
								s.Names[i] = ast.NewIdent("_")
							}
						}
					}
				}
			}
		}
		files = append(files, file)
	}
	if errList != nil {
		return nil, errList
	}
	return files, nil
}

type Options struct {
	GOROOT        string
	GOPATH        string
	Verbose       bool
	Watch         bool
	CreateMapFile bool
	Minify        bool
}

type PackageData struct {
	*build.Package
	SrcModTime time.Time
	UpToDate   bool
	Archive    *compiler.Archive
}

type Session struct {
	options       *Options
	Packages      map[string]*PackageData
	ImportContext *compiler.ImportContext
	Watcher       *fsnotify.Watcher
}

func NewSession(options *Options) *Session {
	if options.GOROOT == "" {
		options.GOROOT = build.Default.GOROOT
	}
	if options.GOPATH == "" {
		options.GOPATH = build.Default.GOPATH
	}
	options.Verbose = options.Verbose || options.Watch

	s := &Session{
		options:  options,
		Packages: make(map[string]*PackageData),
	}
	s.ImportContext = compiler.NewImportContext(s.ImportPackage)
	if options.Watch {
		if out, err := exec.Command("ulimit", "-n").Output(); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n < 1024 {
				fmt.Printf("Warning: The maximum number of open file descriptors is very low (%d). Change it with 'ulimit -n 8192'.\n", n)
			}
		}

		var err error
		s.Watcher, err = fsnotify.NewWatcher()
		if err != nil {
			panic(err)
		}
	}
	return s
}

func (s *Session) ArchSuffix() string {
	if s.options.Minify {
		return "js-min"
	}
	return "js"
}

func (s *Session) BuildDir(packagePath string, importPath string, pkgObj string) error {
	if s.Watcher != nil {
		s.Watcher.Add(packagePath)
	}
	buildPkg, err := NewBuildContext(s.ArchSuffix()).ImportDir(packagePath, 0)
	if err != nil {
		return err
	}
	pkg := &PackageData{Package: buildPkg}
	pkg.ImportPath = importPath
	if err := s.BuildPackage(pkg); err != nil {
		return err
	}
	if pkgObj == "" {
		pkgObj = filepath.Base(packagePath) + ".js"
	}
	if err := s.WriteCommandPackage(pkg, pkgObj); err != nil {
		return err
	}
	return nil
}

func (s *Session) BuildFiles(filenames []string, pkgObj string, packagePath string) error {
	pkg := &PackageData{
		Package: &build.Package{
			Name:       "main",
			ImportPath: "main",
			Dir:        packagePath,
			GoFiles:    filenames,
		},
	}

	if err := s.BuildPackage(pkg); err != nil {
		return err
	}
	if s.ImportContext.Packages["main"].Name() != "main" {
		return fmt.Errorf("cannot build/run non-main package")
	}
	return s.WriteCommandPackage(pkg, pkgObj)
}

func (s *Session) ImportPackage(path string) (*compiler.Archive, error) {
	if pkg, found := s.Packages[path]; found {
		return pkg.Archive, nil
	}

	buildPkg, err := Import(path, build.AllowBinary, s.ArchSuffix())
	if s.Watcher != nil && buildPkg != nil { // add watch even on error
		s.Watcher.Add(buildPkg.Dir)
	}
	if err != nil {
		return nil, err
	}
	pkg := &PackageData{Package: buildPkg}
	if err := s.BuildPackage(pkg); err != nil {
		return nil, err
	}
	return pkg.Archive, nil
}

func (s *Session) ImportDependencies(archive *compiler.Archive) ([]*compiler.Archive, error) {
	var deps []*compiler.Archive
	for _, depPath := range archive.Dependencies {
		dep, err := s.ImportPackage(string(depPath))
		if err != nil {
			return nil, err
		}
		deps = append(deps, dep)
	}
	deps = append(deps, archive)
	return deps, nil
}

func (s *Session) BuildPackage(pkg *PackageData) error {
	s.Packages[pkg.ImportPath] = pkg
	if pkg.ImportPath == "unsafe" {
		return nil
	}

	if pkg.PkgObj != "" {
		var fileInfo os.FileInfo
		gopherjsBinary, err := osext.Executable()
		if err == nil {
			fileInfo, err = os.Stat(gopherjsBinary)
			if err == nil {
				pkg.SrcModTime = fileInfo.ModTime()
			}
		}
		if err != nil {
			os.Stderr.WriteString("Could not get GopherJS binary's modification timestamp. Please report issue.\n")
			pkg.SrcModTime = time.Now()
		}

		for _, importedPkgPath := range pkg.Imports {
			ignored := true
			for _, pos := range pkg.ImportPos[importedPkgPath] {
				importFile := filepath.Base(pos.Filename)
				for _, file := range pkg.GoFiles {
					if importFile == file {
						ignored = false
						break
					}
				}
				if !ignored {
					break
				}
			}
			if importedPkgPath == "unsafe" || ignored {
				continue
			}
			_, err := s.ImportPackage(importedPkgPath)
			if err != nil {
				return err
			}
			impModeTime := s.Packages[importedPkgPath].SrcModTime
			if impModeTime.After(pkg.SrcModTime) {
				pkg.SrcModTime = impModeTime
			}
		}

		for _, name := range pkg.GoFiles {
			fileInfo, err := os.Stat(filepath.Join(pkg.Dir, name))
			if err != nil {
				return err
			}
			if fileInfo.ModTime().After(pkg.SrcModTime) {
				pkg.SrcModTime = fileInfo.ModTime()
			}
		}

		pkgObjFileInfo, err := os.Stat(pkg.PkgObj)
		if err == nil && !pkg.SrcModTime.After(pkgObjFileInfo.ModTime()) {
			// package object is up to date, load from disk if library
			pkg.UpToDate = true
			if pkg.IsCommand() {
				return nil
			}

			objFile, err := ioutil.ReadFile(pkg.PkgObj)
			if err != nil {
				return err
			}

			pkg.Archive, err = compiler.UnmarshalArchive(pkg.PkgObj, pkg.ImportPath, objFile, s.ImportContext)
			if err != nil {
				return err
			}

			return nil
		}
	}

	fileSet := token.NewFileSet()
	files, err := Parse(pkg.Package, fileSet)
	if err != nil {
		return err
	}
	pkg.Archive, err = compiler.Compile(pkg.ImportPath, files, fileSet, s.ImportContext, s.options.Minify)
	if err != nil {
		return err
	}

	if s.options.Verbose {
		fmt.Println(pkg.ImportPath)
	}

	if pkg.PkgObj == "" || pkg.IsCommand() {
		return nil
	}

	if err := s.writeLibraryPackage(pkg, pkg.PkgObj); err != nil {
		if strings.HasPrefix(pkg.PkgObj, s.options.GOROOT) {
			// fall back to first GOPATH workspace
			firstGopathWorkspace := filepath.SplitList(s.options.GOPATH)[0]
			if err := s.writeLibraryPackage(pkg, filepath.Join(firstGopathWorkspace, pkg.PkgObj[len(s.options.GOROOT):])); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	return nil
}

func (s *Session) writeLibraryPackage(pkg *PackageData, pkgObj string) error {
	if err := os.MkdirAll(filepath.Dir(pkgObj), 0777); err != nil {
		return err
	}

	data, err := compiler.MarshalArchive(pkg.Archive)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(pkgObj, data, 0666)
}

func (s *Session) WriteCommandPackage(pkg *PackageData, pkgObj string) error {
	if !pkg.IsCommand() || pkg.UpToDate {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(pkgObj), 0777); err != nil {
		return err
	}
	codeFile, err := os.Create(pkgObj)
	if err != nil {
		return err
	}
	defer codeFile.Close()

	sourceMapFilter := &compiler.SourceMapFilter{Writer: codeFile}
	if s.options.CreateMapFile {
		m := sourcemap.Map{File: filepath.Base(pkgObj)}
		mapFile, err := os.Create(pkgObj + ".map")
		if err != nil {
			return err
		}

		defer func() {
			m.WriteTo(mapFile)
			mapFile.Close()
			fmt.Fprintf(codeFile, "//# sourceMappingURL=%s.map\n", filepath.Base(pkgObj))
		}()

		sourceMapFilter.MappingCallback = func(generatedLine, generatedColumn int, fileSet *token.FileSet, originalPos token.Pos) {
			if !originalPos.IsValid() {
				m.AddMapping(&sourcemap.Mapping{GeneratedLine: generatedLine, GeneratedColumn: generatedColumn})
				return
			}
			pos := fileSet.Position(originalPos)
			file := pos.Filename
			switch hasGopathPrefix, prefixLen := hasGopathPrefix(file, s.options.GOPATH); {
			case hasGopathPrefix:
				file = filepath.ToSlash(filepath.Join("/gopath", file[prefixLen:]))
			case strings.HasPrefix(file, s.options.GOROOT):
				file = filepath.ToSlash(filepath.Join("/goroot", file[len(s.options.GOROOT):]))
			default:
				file = filepath.Base(file)
			}
			m.AddMapping(&sourcemap.Mapping{GeneratedLine: generatedLine, GeneratedColumn: generatedColumn, OriginalFile: file, OriginalLine: pos.Line, OriginalColumn: pos.Column})
		}
	}

	deps, err := s.ImportDependencies(pkg.Archive)
	if err != nil {
		return err
	}
	return compiler.WriteProgramCode(deps, s.ImportContext, sourceMapFilter)
}

// hasGopathPrefix returns true and the length of the matched GOPATH workspace,
// iff file has a prefix that matches one of the GOPATH workspaces.
func hasGopathPrefix(file, gopath string) (hasGopathPrefix bool, prefixLen int) {
	gopathWorkspaces := filepath.SplitList(gopath)
	for _, gopathWorkspace := range gopathWorkspaces {
		if strings.HasPrefix(file, gopathWorkspace) {
			return true, len(gopathWorkspace)
		}
	}
	return false, 0
}

func (s *Session) WaitForChange() {
	fmt.Println("\x1B[32mwatching for changes...\x1B[39m")
	select {
	case ev := <-s.Watcher.Events:
		fmt.Println("\x1B[32mchange detected: " + ev.Name + "\x1B[39m")
	case err := <-s.Watcher.Errors:
		fmt.Println("\x1B[32mwatcher error: " + err.Error() + "\x1B[39m")
	}
	s.Watcher.Close()
}
