// Package architecture implements the Architecture Mapper: the internal
// module dependency graph, circular dependencies, layering violations,
// coupling and a Mermaid diagram.
package architecture

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/capybari/capybari-core/analyzer"
	"github.com/capybari/capybari-core/facts"
	"github.com/capybari/capybari-core/finding"
	"github.com/capybari/capybari-core/fsutil"
)

//go:embed capability.yaml
var capabilityYAML []byte

var capability = analyzer.MustParseCapability(capabilityYAML)

const (
	maxCycleFindings = 10
	maxLayerFindings = 10
	fanOutThreshold  = 12
	diagramNodes     = 25
)

// Analyzer implements the capability.
type Analyzer struct{}

// New returns the capability.
func New() *Analyzer { return &Analyzer{} }

// Capability implements analyzer.Analyzer.
func (*Analyzer) Capability() analyzer.Capability { return capability }

var supported = map[string]bool{"Go": true, "JavaScript": true, "TypeScript": true, "Vue": true, "Svelte": true, "Python": true, "Java": true, "Kotlin": true, "C#": true, "PHP": true}

// Applies declines for repositories without a supported language.
func (*Analyzer) Applies(in *analyzer.Input) (bool, string) {
	var inv facts.Inventory
	if ok, _ := in.Evidence.Get(facts.KeyInventory, &inv); !ok {
		return false, "no inventory"
	}
	for _, l := range inv.Languages {
		if supported[l.Language] {
			return true, ""
		}
	}
	return false, "no source in a supported language (Go, JavaScript/TypeScript, Python, Java/Kotlin, C#, PHP)"
}

// edge is a resolved internal dependency between units.
type edge struct {
	from, to string // unit IDs
	file     string // importing file
	line     int
	fromDir  string
	toDir    string
}

// graph accumulates units (cycle granularity) and directory nodes.
type graph struct {
	edges    []edge
	unitDir  map[string]string // unit -> directory
	dirLang  map[string]string
	dirFiles map[string]int
	dirLines map[string]int
	external map[string]int
}

// Analyze implements analyzer.Analyzer.
func (*Analyzer) Analyze(ctx context.Context, in *analyzer.Input) (*analyzer.Result, error) {
	var inv facts.Inventory
	if _, err := in.Evidence.Get(facts.KeyInventory, &inv); err != nil {
		return nil, err
	}
	root := in.Target.Root
	files := map[string]facts.File{}
	for _, f := range inv.Files {
		files[f.Path] = f
	}
	exists := func(p string) bool {
		f, ok := files[p]
		return ok && (f.Kind == facts.KindSource || f.Kind == facts.KindTest || f.Kind == facts.KindGenerated)
	}
	g := &graph{unitDir: map[string]string{}, dirLang: map[string]string{}, dirFiles: map[string]int{}, dirLines: map[string]int{}, external: map[string]int{}}

	goMods := goModules(root, &inv)
	pyRoots := pythonRoots(&inv)
	javaPkgs := map[string]string{} // package/namespace -> representative dir
	type pending struct {
		file  facts.File
		unit  string
		specs []rawImport
		kind  string
	}
	var work []pending

	for _, f := range inv.Filter(facts.KindSource) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !supported[f.Language] || f.Size > fsutil.DefaultMaxRead {
			continue
		}
		src, _, err := fsutil.ReadFile(root, f.Path, fsutil.DefaultMaxRead)
		if err != nil {
			continue
		}
		dir := path.Dir(f.Path)
		g.dirFiles[dir]++
		g.dirLines[dir] += f.Lines
		if g.dirLang[dir] == "" {
			g.dirLang[dir] = f.Language
		}
		s := string(src)
		switch f.Language {
		case "Go":
			work = append(work, pending{f, "go:" + dir, goImports(src), "go"})
			g.unitDir["go:"+dir] = dir
		case "JavaScript", "TypeScript", "Vue", "Svelte":
			work = append(work, pending{f, "file:" + f.Path, jsImports(s), "js"})
			g.unitDir["file:"+f.Path] = dir
		case "Python":
			work = append(work, pending{f, "file:" + f.Path, pyImports(s), "py"})
			g.unitDir["file:"+f.Path] = dir
		case "Java", "Kotlin":
			pkg := ""
			if m := javaPkg.FindStringSubmatch(s); m != nil {
				pkg = m[1]
				javaPkgs[pkg] = dir
			}
			work = append(work, pending{f, "ns:" + pkg, regexImports(javaImport, s, 1), "java"})
			g.unitDir["ns:"+pkg] = dir
		case "C#":
			ns := ""
			if m := csNS.FindStringSubmatch(s); m != nil {
				ns = m[1]
				javaPkgs[ns] = dir
			}
			work = append(work, pending{f, "ns:" + ns, regexImports(csUsing, s, 1), "cs"})
			g.unitDir["ns:"+ns] = dir
		case "PHP":
			ns := ""
			if m := phpNS.FindStringSubmatch(s); m != nil {
				ns = strings.ReplaceAll(m[1], `\`, ".")
				javaPkgs[ns] = dir
			}
			var specs []rawImport
			for _, r := range regexImports(phpUse, s, 1) {
				specs = append(specs, rawImport{strings.ReplaceAll(r.spec, `\`, "."), r.line})
			}
			work = append(work, pending{f, "ns:" + ns, specs, "php"})
			g.unitDir["ns:"+ns] = dir
		}
	}

	for _, w := range work {
		fromDir := path.Dir(w.file.Path)
		for _, im := range w.specs {
			var to, toDir string
			switch w.kind {
			case "go":
				if d, ok := resolveGo(im.spec, goMods); ok {
					to, toDir = "go:"+d, d
				} else if first := strings.SplitN(im.spec, "/", 2)[0]; strings.Contains(first, ".") {
					g.external[goModuleRoot(im.spec)]++
				}
			case "js":
				if p, ok := jsResolve(w.file.Path, im.spec, exists); ok {
					to, toDir = "file:"+p, path.Dir(p)
				} else if pkg := jsPackage(im.spec); pkg != "" && !strings.HasPrefix(im.spec, "@/") && !strings.HasPrefix(im.spec, "~/") {
					g.external[pkg]++
				}
			case "py":
				if p, ok := pyResolve(w.file.Path, im.spec, exists, pyRoots); ok {
					// Importing your own package ("from . import x", "from pkg
					// import y" inside pkg) goes through __init__.py re-exports;
					// that is Python's normal package pattern, not a cycle.
					if path.Base(p) == "__init__.py" && strings.HasPrefix(w.file.Path+"/", path.Dir(p)+"/") {
						continue
					}
					to, toDir = "file:"+p, path.Dir(p)
				}
			case "java", "cs", "php":
				spec := im.spec
				for spec != "" {
					if d, ok := javaPkgs[spec]; ok {
						to, toDir = "ns:"+spec, d
						break
					}
					i := strings.LastIndex(spec, ".")
					if i < 0 {
						break
					}
					spec = spec[:i]
				}
			}
			if to == "" || to == w.unit {
				continue
			}
			g.edges = append(g.edges, edge{from: w.unit, to: to, file: w.file.Path, line: im.line, fromDir: fromDir, toDir: toDir})
		}
	}

	arch, findings := g.analyse()
	res := &analyzer.Result{
		Evidence: map[string]any{facts.KeyArchitecture: arch},
		Findings: findings,
		Summary:  fmt.Sprintf("%d modules, %d internal dependencies, %d cycle(s), %d external packages", len(arch.Nodes), len(arch.Edges), len(arch.Cycles), len(arch.External)),
		Limitations: []string{
			"Dependencies are derived from static import statements; dynamic imports, dependency injection and reflection are not visible.",
		},
	}
	return res, nil
}

func (g *graph) analyse() (*facts.Architecture, []finding.Finding) {
	arch := &facts.Architecture{}
	var findings []finding.Finding

	// Unit-level cycles (files for JS/Python, packages/namespaces otherwise).
	adj := map[string]map[string]edge{}
	for _, e := range g.edges {
		if adj[e.from] == nil {
			adj[e.from] = map[string]edge{}
		}
		if _, ok := adj[e.from][e.to]; !ok {
			adj[e.from][e.to] = e
		}
	}
	sccs := tarjan(adj)
	for _, scc := range sccs {
		var names []string
		for _, u := range scc {
			names = append(names, unitName(u))
		}
		arch.Cycles = append(arch.Cycles, names)
	}
	sort.Slice(arch.Cycles, func(i, j int) bool {
		if len(arch.Cycles[i]) != len(arch.Cycles[j]) {
			return len(arch.Cycles[i]) > len(arch.Cycles[j])
		}
		return strings.Join(arch.Cycles[i], ",") < strings.Join(arch.Cycles[j], ",")
	})
	for i, scc := range sccs {
		if i == maxCycleFindings {
			break
		}
		in := map[string]bool{}
		for _, u := range scc {
			in[u] = true
		}
		var ev []finding.Evidence
		dirs := map[string]bool{}
		for _, u := range scc {
			dirs[g.unitDir[u]] = true
			var targets []string
			for t := range adj[u] {
				if in[t] {
					targets = append(targets, t)
				}
			}
			sort.Strings(targets)
			for _, t := range targets {
				e := adj[u][t]
				ev = append(ev, finding.Evidence{Location: finding.Location{Path: e.file, StartLine: e.line}, Detail: unitName(u) + " → " + unitName(t)})
			}
		}
		sev := finding.Medium
		if len(dirs) == 1 && len(scc) == 2 {
			sev = finding.Low
		}
		names := make([]string, 0, len(scc))
		for _, u := range scc {
			names = append(names, unitName(u))
		}
		findings = append(findings, finding.Finding{
			Dimension: finding.DimStructure, Category: "dependency-cycle", Severity: sev, Confidence: finding.ConfidenceHigh,
			Title:       fmt.Sprintf("Circular dependency between %d modules", len(scc)),
			Description: fmt.Sprintf("%s depend on each other in a cycle. None of them can be changed, tested or extracted in isolation.", strings.Join(names, ", ")),
			Evidence:    ev,
			Rule:        &finding.Rule{ID: "dependency-cycle"},
			Impact:      &finding.Impact{Technical: "Changes ripple through every module in the cycle; initialisation order bugs in JavaScript and Python."},
			Remediation: &finding.Remediation{Summary: "Break the cycle by moving the shared piece into a separate module both depend on, or invert one dependency through an interface or callback.", Automatable: false},
		})
	}

	// Directory-level graph for the diagram, coupling and layers.
	type dk struct{ from, to string }
	weights := map[dk]int{}
	firstEdge := map[dk]edge{}
	for _, e := range g.edges {
		if e.fromDir == e.toDir {
			continue
		}
		k := dk{e.fromDir, e.toDir}
		if weights[k] == 0 {
			firstEdge[k] = e
		}
		weights[k]++
	}
	fanOut, fanIn := map[string]int{}, map[string]int{}
	for k, w := range weights {
		arch.Edges = append(arch.Edges, facts.ArchEdge{From: k.from, To: k.to, Weight: w})
		fanOut[k.from]++
		fanIn[k.to]++
	}
	sort.Slice(arch.Edges, func(i, j int) bool {
		if arch.Edges[i].From != arch.Edges[j].From {
			return arch.Edges[i].From < arch.Edges[j].From
		}
		return arch.Edges[i].To < arch.Edges[j].To
	})
	for dir, n := range g.dirFiles {
		arch.Nodes = append(arch.Nodes, facts.ArchNode{ID: dir, Language: g.dirLang[dir], Files: n, Lines: g.dirLines[dir], Layer: layerOf(dir)})
	}
	sort.Slice(arch.Nodes, func(i, j int) bool { return arch.Nodes[i].ID < arch.Nodes[j].ID })

	// Layer violations: inner layers must not depend on outer ones.
	var violations []finding.Finding
	keys := make([]dk, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		return keys[i].to < keys[j].to
	})
	for _, k := range keys {
		lf, lt := layerOf(k.from), layerOf(k.to)
		if lf == "" || lt == "" || layerRank[lf] >= layerRank[lt] {
			continue
		}
		e := firstEdge[k]
		violations = append(violations, finding.Finding{
			Dimension: finding.DimStructure, Category: "layer-violation", Severity: finding.Low, Confidence: finding.ConfidenceMedium,
			Title:                 fmt.Sprintf("%s layer depends on %s layer (%s → %s)", lf, lt, k.from, k.to),
			Description:           fmt.Sprintf("%s (%s) imports %s (%s) %d time(s). Inner layers should not depend on outer ones.", k.from, lf, k.to, lt, weights[k]),
			Evidence:              []finding.Evidence{{Location: finding.Location{Path: e.file, StartLine: e.line}}},
			Rule:                  &finding.Rule{ID: "layer-violation"},
			Remediation:           &finding.Remediation{Summary: "Move the shared type or logic inward, or pass it in from the outer layer.", Automatable: false},
			FalsePositiveGuidance: "Layers are inferred from directory names (routes/controllers/handlers → interface, services/domain → domain, models/db/repositories → data, utils/lib/common → shared). Projects with other conventions may be misclassified.",
		})
	}
	if len(violations) > maxLayerFindings {
		violations = violations[:maxLayerFindings]
	}
	findings = append(findings, violations...)

	var busy []string
	for d, n := range fanOut {
		if n > fanOutThreshold {
			busy = append(busy, d)
		}
	}
	sort.Slice(busy, func(i, j int) bool { return fanOut[busy[i]] > fanOut[busy[j]] })
	for i, d := range busy {
		if i == 5 {
			break
		}
		findings = append(findings, finding.Finding{
			Dimension: finding.DimStructure, Category: "high-coupling", Severity: finding.Low, Confidence: finding.ConfidenceHigh,
			Title:       fmt.Sprintf("%s depends on %d other modules", d, fanOut[d]),
			Evidence:    []finding.Evidence{{Location: finding.Location{Path: d + "/"}}},
			Rule:        &finding.Rule{ID: "high-fan-out"},
			Remediation: &finding.Remediation{Summary: "Split the module by responsibility, or introduce a facade so it depends on fewer modules directly.", Automatable: false},
		})
	}

	for pkg := range g.external {
		arch.External = append(arch.External, pkg)
	}
	sort.Slice(arch.External, func(i, j int) bool {
		if g.external[arch.External[i]] != g.external[arch.External[j]] {
			return g.external[arch.External[i]] > g.external[arch.External[j]]
		}
		return arch.External[i] < arch.External[j]
	})
	arch.Mermaid = mermaid(arch, fanIn, fanOut)
	return arch, findings
}

func unitName(u string) string {
	if i := strings.Index(u, ":"); i >= 0 {
		return u[i+1:]
	}
	return u
}

var layerRank = map[string]int{"interface": 3, "domain": 2, "data": 1, "shared": 0}

var layerWords = []struct {
	layer string
	words []string
}{
	{"interface", []string{"routes", "router", "controllers", "controller", "handlers", "handler", "api", "views", "pages", "cmd", "cli", "http", "web", "endpoints", "resolvers", "ui", "components"}},
	{"domain", []string{"services", "service", "domain", "usecases", "usecase", "core", "business", "logic", "app"}},
	{"data", []string{"models", "model", "entities", "entity", "db", "database", "repositories", "repository", "repo", "store", "storage", "dao", "persistence", "migrations"}},
	{"shared", []string{"utils", "util", "lib", "libs", "common", "shared", "helpers", "helper", "pkg"}},
}

// layerOf infers a layer from the deepest directory segment that names one.
func layerOf(dir string) string {
	segs := strings.Split(strings.ToLower(dir), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		for _, lw := range layerWords {
			for _, w := range lw.words {
				if segs[i] == w {
					return lw.layer
				}
			}
		}
	}
	return ""
}

var mermaidID = regexp.MustCompile(`[^A-Za-z0-9_]`)

// mermaid renders the busiest directories as a left-to-right graph.
func mermaid(a *facts.Architecture, fanIn, fanOut map[string]int) string {
	if len(a.Nodes) == 0 {
		return ""
	}
	nodes := append([]facts.ArchNode(nil), a.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		si := fanIn[nodes[i].ID] + fanOut[nodes[i].ID]
		sj := fanIn[nodes[j].ID] + fanOut[nodes[j].ID]
		if si != sj {
			return si > sj
		}
		return nodes[i].Lines > nodes[j].Lines
	})
	keep := map[string]bool{}
	for i, n := range nodes {
		if i == diagramNodes {
			break
		}
		keep[n.ID] = true
	}
	id := func(s string) string { return "n_" + mermaidID.ReplaceAllString(s, "_") }
	var b bytes.Buffer
	b.WriteString("graph LR\n")
	for _, n := range a.Nodes {
		if !keep[n.ID] {
			continue
		}
		label := n.ID
		if label == "." {
			label = "(root)"
		}
		fmt.Fprintf(&b, "  %s[\"%s<br/>%d files\"]\n", id(n.ID), label, n.Files)
	}
	cyc := map[string]bool{}
	for _, c := range a.Cycles {
		for _, u := range c {
			cyc[path.Dir(u)] = true
			cyc[u] = true
		}
	}
	for _, e := range a.Edges {
		if keep[e.From] && keep[e.To] {
			arrow := "-->"
			if cyc[e.From] && cyc[e.To] {
				arrow = "-.->"
			}
			fmt.Fprintf(&b, "  %s %s|%d| %s\n", id(e.From), arrow, e.Weight, id(e.To))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// tarjan returns strongly connected components with more than one member.
func tarjan(adj map[string]map[string]edge) [][]string {
	index := 0
	idx, low := map[string]int{}, map[string]int{}
	on := map[string]bool{}
	var stack []string
	var out [][]string
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	var strong func(v string)
	strong = func(v string) {
		idx[v], low[v] = index, index
		index++
		stack = append(stack, v)
		on[v] = true
		succ := make([]string, 0, len(adj[v]))
		for w := range adj[v] {
			succ = append(succ, w)
		}
		sort.Strings(succ)
		for _, w := range succ {
			if _, seen := idx[w]; !seen {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], idx[w])
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Strings(comp)
				out = append(out, comp)
			}
		}
	}
	for _, n := range nodes {
		if _, seen := idx[n]; !seen {
			strong(n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// goModules maps module paths to their directories.
func goModules(root string, inv *facts.Inventory) map[string]string {
	mods := map[string]string{}
	for _, f := range inv.Files {
		if path.Base(f.Path) != "go.mod" || f.Kind == facts.KindVendored {
			continue
		}
		b, _, err := fsutil.ReadFile(root, f.Path, 1<<20)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "module ") {
				mods[strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)] = path.Dir(f.Path)
				break
			}
		}
	}
	return mods
}

func resolveGo(imp string, mods map[string]string) (string, bool) {
	best := ""
	for m := range mods {
		if (imp == m || strings.HasPrefix(imp, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return "", false
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(imp, best), "/")
	d := path.Clean(path.Join(mods[best], rel))
	return d, true
}

func goModuleRoot(imp string) string {
	parts := strings.Split(imp, "/")
	if len(parts) >= 3 && (parts[0] == "github.com" || parts[0] == "gitlab.com" || parts[0] == "bitbucket.org") {
		return strings.Join(parts[:3], "/")
	}
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/")
	}
	return imp
}

// pythonRoots returns candidate import roots: the repository root and
// conventional source directories.
func pythonRoots(inv *facts.Inventory) []string {
	roots := []string{"."}
	seen := map[string]bool{".": true}
	for _, f := range inv.Files {
		if !strings.HasSuffix(f.Path, ".py") {
			continue
		}
		for _, r := range []string{"src", "lib", "app"} {
			if strings.HasPrefix(f.Path, r+"/") && !seen[r] {
				seen[r] = true
				roots = append(roots, r)
			}
		}
	}
	return roots
}
