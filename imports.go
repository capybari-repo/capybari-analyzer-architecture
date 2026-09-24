package architecture

import (
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// rawImport is one import statement before resolution.
type rawImport struct {
	spec string
	line int
}

var (
	jsImport   = regexp.MustCompile(`(?m)(?:^|[^\w.$])(?:import\s+(?:[\w*{}\s,$]+\s+from\s+)?|export\s+(?:[\w*{}\s,$]+\s+from\s+)|require\s*\(\s*|import\s*\(\s*)["']([^"'\n]+)["']`)
	pyImport   = regexp.MustCompile(`(?m)^\s*(?:from\s+(\.*[\w.]*)\s+import\s+([\w*, ()]+)|import\s+([\w., ]+))`)
	javaImport = regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([\w.]+)(?:\.\*)?\s*;?`)
	javaPkg    = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;?`)
	csUsing    = regexp.MustCompile(`(?m)^\s*(?:global\s+)?using\s+(?:static\s+)?([\w.]+)\s*;`)
	csNS       = regexp.MustCompile(`(?m)^\s*namespace\s+([\w.]+)`)
	phpUse     = regexp.MustCompile(`(?m)^\s*use\s+(?:function\s+|const\s+)?([\w\\]+)`)
	phpNS      = regexp.MustCompile(`(?m)^\s*namespace\s+([\w\\]+)\s*;`)
)

func lineAt(src string, idx int) int { return strings.Count(src[:idx], "\n") + 1 }

// goImports returns import paths using the real Go parser.
func goImports(src []byte) []rawImport {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []rawImport
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err == nil {
			out = append(out, rawImport{p, fset.Position(im.Pos()).Line})
		}
	}
	return out
}

var typeOnly = regexp.MustCompile(`(?m)^\s*(?:import|export)\s+type\s[^\n]*$`)

// jsImports ignores TypeScript type-only imports, which are erased at
// compile time and create no runtime dependency.
func jsImports(src string) []rawImport {
	src = typeOnly.ReplaceAllStringFunc(src, func(m string) string { return strings.Repeat(" ", len(m)) })
	return regexImports(jsImport, src, 1)
}

func regexImports(re *regexp.Regexp, src string, groups ...int) []rawImport {
	var out []rawImport
	for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
		for _, g := range groups {
			if m[2*g] >= 0 {
				out = append(out, rawImport{src[m[2*g]:m[2*g+1]], lineAt(src, m[0])})
				break
			}
		}
	}
	return out
}

// pyImports expands "from pkg import a, b" into candidate module specs,
// since "a" may itself be a submodule of pkg.
func pyImports(src string) []rawImport {
	// Only module-level imports create load-time dependencies. Indented
	// imports are either under "if TYPE_CHECKING:" (type-only) or inside
	// functions (deliberately deferred), so they are blanked out.
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			lines[i] = ""
		}
	}
	src = strings.Join(lines, "\n")
	var out []rawImport
	for _, m := range pyImport.FindAllStringSubmatchIndex(src, -1) {
		line := lineAt(src, m[0])
		if m[2] >= 0 {
			mod := src[m[2]:m[3]]
			names := strings.Trim(src[m[4]:m[5]], "() ")
			out = append(out, rawImport{mod, line})
			for _, n := range strings.Split(names, ",") {
				n = strings.TrimSpace(strings.SplitN(strings.TrimSpace(n), " ", 2)[0])
				if n != "" && n != "*" {
					sep := "."
					if strings.HasSuffix(mod, ".") {
						sep = ""
					}
					out = append(out, rawImport{mod + sep + n + "\x00sub", line})
				}
			}
			continue
		}
		for _, n := range strings.Split(src[m[6]:m[7]], ",") {
			n = strings.TrimSpace(strings.SplitN(strings.TrimSpace(n), " ", 2)[0])
			if n != "" {
				out = append(out, rawImport{n, line})
			}
		}
	}
	return out
}

// jsResolve resolves a relative JS/TS specifier against the importing file.
func jsResolve(from, spec string, exists func(string) bool) (string, bool) {
	if !strings.HasPrefix(spec, ".") {
		return "", false
	}
	base := path.Clean(path.Join(path.Dir(from), spec))
	cands := []string{base}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue", ".svelte"} {
		cands = append(cands, base+ext)
	}
	for _, idx := range []string{"/index.ts", "/index.tsx", "/index.js", "/index.jsx", "/index.mjs"} {
		cands = append(cands, base+idx)
	}
	// TS projects import "./x.js" that is compiled from "./x.ts".
	if strings.HasSuffix(base, ".js") {
		cands = append(cands, strings.TrimSuffix(base, ".js")+".ts", strings.TrimSuffix(base, ".js")+".tsx")
	}
	for _, c := range cands {
		if exists(c) {
			return c, true
		}
	}
	return "", false
}

// jsPackage returns the npm package name of a bare specifier.
func jsPackage(spec string) string {
	if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		return ""
	}
	if strings.HasPrefix(spec, "node:") {
		return ""
	}
	parts := strings.Split(spec, "/")
	if strings.HasPrefix(spec, "@") && len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// pyResolve resolves a Python module spec to a repository file.
func pyResolve(from, spec string, exists func(string) bool, roots []string) (string, bool) {
	spec = strings.TrimSuffix(spec, "\x00sub")
	dots := len(spec) - len(strings.TrimLeft(spec, "."))
	mod := strings.TrimLeft(spec, ".")
	var bases []string
	if dots > 0 {
		dir := path.Dir(from)
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		bases = []string{dir}
	} else {
		bases = roots
	}
	rel := strings.ReplaceAll(mod, ".", "/")
	for _, b := range bases {
		p := path.Join(b, rel)
		if mod == "" {
			p = b
		}
		for _, c := range []string{p + ".py", p + "/__init__.py"} {
			if exists(c) {
				return c, true
			}
		}
	}
	return "", false
}
