# Methodology: Architecture Mapper

## Graph

Only source files are parsed. Tests, generated and vendored code are excluded.

| Language | Unit (cycle granularity) | Resolution |
|---|---|---|
| Go | package directory | `go/parser` imports; internal when prefixed by a `module` path from any `go.mod` |
| JavaScript/TypeScript/Vue/Svelte | file | relative specifiers resolved with extension, `index` and `.js`→`.ts` rules; bare specifiers are external packages |
| Python | file | absolute (repository root, `src/`, `lib/`, `app/`) and relative imports; `from pkg import sub` tries `pkg/sub` |
| Java/Kotlin | package | `package` declarations map packages to directories; imports resolved by longest known prefix |
| C# | namespace | `namespace` / `using` |
| PHP | namespace | `namespace` / `use` |

For the diagram and coupling, units are aggregated to directories. Edges within one directory are omitted there, but still count for cycles.

## Findings

| Rule | Severity | Confidence |
|---|---|---|
| `dependency-cycle` | medium; low when the cycle is two units in one directory | high |
| `layer-violation` | low | medium (layers are inferred from names) |
| `high-fan-out` (> 12 modules) | low | high |

Layer order, from outermost to innermost: **interface** (routes, controllers, handlers, api, views, pages, cmd, cli, http, web, ui, components) → **domain** (services, domain, usecases, core, business, app) → **data** (models, entities, db, repositories, store, dao, persistence, migrations) → **shared** (utils, lib, common, helpers, pkg). A dependency from an inner layer to an outer one is a violation.

## Limitations

Static imports only: dynamic loading, dependency injection containers and reflection are invisible. Monorepo path aliases (`@/…`, `~/…`) are not yet resolved.
