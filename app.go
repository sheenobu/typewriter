package typewriter

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"sort"

	"golang.org/x/tools/imports"
)

// App is the high-level construct for package-level code generation. Typical usage is along the lines of:
//	app, err := typewriter.NewApp()
//	err := app.WriteAll()
//
// +test foo:"Bar" baz:"qux[struct{}],thing"
type App struct {
	// All typewriter.Package found in the current directory.
	Packages []*Package
	// All typewriter.Interface's registered on init.
	TypeWriters []Interface
	Directive   string
}

// NewApp parses the current directory, enumerating registered TypeWriters and collecting Types and their related information.
func NewApp(directive string) (*App, error) {
	return DefaultConfig.NewApp(directive)
}

func (conf *Config) NewApp(directive string) (*App, error) {
	a := &App{
		Directive:   directive,
		TypeWriters: typeWriters,
	}

	pkgs, err := getPackages(directive, conf)

	a.Packages = pkgs
	return a, err
}

// NewAppFiltered parses the current directory, collecting Types and their related information. Pass a filter to limit which files are operated on.
func NewAppFiltered(directive string, filter func(os.FileInfo) bool) (*App, error) {
	conf := &Config{
		Filter: filter,
	}
	return conf.NewApp(directive)
}

// Individual TypeWriters register on init, keyed by name
var typeWriters []Interface

// Register allows template packages to make themselves known to a 'parent' package, usually in the init() func.
// Comparable to the approach taken by stdlib's image package for registration of image types (eg image/png).
// Your program will do something like:
//	import (
//		"github.com/clipperhouse/typewriter"
//		_ "github.com/clipperhouse/slice"
//	)
func Register(tw Interface) error {
	for _, v := range typeWriters {
		if v.Name() == tw.Name() {
			return fmt.Errorf("A TypeWriter by the name %s has already been registered", tw.Name())
		}
	}
	typeWriters = append(typeWriters, tw)
	return nil
}

// WriteAll writes the generated code for all Types and TypeWriters in the App to respective files.
func (a *App) WriteAll() ([]string, error) {
	var written []string

	// one map of buffers for each package and one buffer for each file, keyed by file name
	buffers := make(map[string]map[string]*bytes.Buffer)

	// write the generated code for each Type & TypeWriter into memory
	for _, p := range a.Packages {
		pkgName := p.Path() + p.Name() //TODO: Ensure this is unique
		buffers[pkgName] = make(map[string]*bytes.Buffer)

		if p.singleFile {
			// TODO: split up non test types from test types within the package
			for _, tw := range a.TypeWriters {

				// Sort types so that they always appear in stable order in the output file
				sort.Sort(typeByName(p.Types))

				var b bytes.Buffer
				n, err := write(&b, a, p, p.Types, tw)

				if err != nil {
					return written, err
				}

				// don't generate a file if no bytes were written by WriteHeader or WriteBody
				if n == 0 {
					continue
				}

				f := strings.ToLower(fmt.Sprintf("%s_%s.go", p.Name(), tw.Name()))

				buffers[pkgName][f] = &b
			}
		} else {
			for _, t := range p.Types {
				for _, tw := range a.TypeWriters {
					var b bytes.Buffer
					n, err := write(&b, a, p, []Type{t}, tw)

					if err != nil {
						return written, err
					}

					// don't generate a file if no bytes were written by WriteHeader or WriteBody
					if n == 0 {
						continue
					}

					// append _test to file name if the source type is in a _test.go file
					f := strings.ToLower(fmt.Sprintf("%s_%s%s.go", t.Name, tw.Name(), t.test))

					buffers[pkgName][f] = &b
				}
			}
		}
	}

	// validate generated ast's before committing to files
	for _, bm := range buffers {
		for f, b := range bm {
			if _, err := parser.ParseFile(token.NewFileSet(), f, b.String(), 0); err != nil {
				// TODO: prompt to write (ignored) _file on error? parsing errors are meaningless without.
				return written, err
			}
		}
	}

	// format, remove unused imports, and commit to files
	for _, bm := range buffers {
		for f, b := range bm {
			src, err := imports.Process(f, b.Bytes(), nil)

			// shouldn't be an error if the ast parsing above succeeded
			if err != nil {
				return written, err
			}

			if err := writeFile(f, src); err != nil {
				return written, err
			}

			written = append(written, f)
		}
	}

	return written, nil
}

var twoLines = bytes.Repeat([]byte{'\n'}, 2)

func write(w *bytes.Buffer, a *App, p *Package, tx []Type, tw Interface) (n int, err error) {

	if len(tx) == 0 {
		return 0, nil
	}

	// start with byline at top, give future readers some background
	// on where the file came from
	bylineFmt := `// Generated by: %s
// TypeWriter: %s
`
	directiveFmt := "// Directive: %s on %s\n"

	caller := filepath.Base(os.Args[0])

	byline := fmt.Sprintf(bylineFmt, caller, tw.Name())
	w.Write([]byte(byline))

	for _, t := range tx {
		directive := fmt.Sprintf(directiveFmt, a.Directive, t.String())
		w.Write([]byte(directive))
	}

	// add a package declaration
	pkg := fmt.Sprintf("package %s", p.Name())
	w.Write([]byte(pkg))
	w.Write(twoLines)

	// build unique list of imports
	var imports = NewImportSpecSet()
	for _, t := range tx {
		for _, i := range tw.Imports(t) {
			imports.Add(i)
		}
	}

	if err := importsTmpl.Execute(w, imports.ToSlice()); err != nil {
		return n, err
	}

	c := countingWriter{0, w}

	for _, t := range tx {
		err = tw.Write(&c, t)
		n += c.n
		w.Write(twoLines)
	}

	return n, err
}

func writeFile(filename string, byts []byte) error {
	w, err := os.Create(filename)

	if err != nil {
		return err
	}

	defer w.Close()

	w.Write(byts)

	return nil
}

var importsTmpl = template.Must(template.New("imports").Parse(`{{if gt (len .) 0}}
{{range .}}
import {{.Name}} "{{.Path}}"{{end}}
{{end}}
`))

// a writer that knows how much writing it did
// https://groups.google.com/forum/#!topic/golang-nuts/VQLtfRGqK8Q
type countingWriter struct {
	n int
	w io.Writer
}

func (c *countingWriter) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	c.n += n
	return
}
