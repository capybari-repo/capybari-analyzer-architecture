# capybari-analyzer-architecture

**Capybari Source Intelligence: Architecture Mapper: how is this system put together?**

- **Internal dependency graph** from import statements: Go (parser), JavaScript/TypeScript (ES modules, `require`, dynamic `import()`, index/extension resolution), Python (absolute and relative imports, `src/` layouts), Java/Kotlin packages, C# namespaces and PHP namespaces
- **Circular dependencies** (Tarjan's strongly connected components) between files (JS/TS, Python) or packages/namespaces (Go, Java, C#, PHP)
- **Layer violations:** directories are mapped to interface / domain / data / shared layers by name, and inner layers depending on outer layers are flagged
- **High coupling:** modules depending on more than 12 others
- **External packages** actually imported by code (npm, Go modules)
- **Mermaid diagram** of the 25 most connected modules, embedded in the Markdown report

| | |
|---|---|
| Requires | `inventory` |
| Provides | `architecture` evidence (nodes, edges, cycles, external, mermaid) |
| Scores | Structure |
| Network / AI | none / none |

```bash
go run ./cmd/capybari-architecture ./path/to/project
```

## License

Apache-2.0
