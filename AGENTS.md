StockIt is a high-performance, self-contained (Asset Bundling) warehouse management app built in Go with UI Layer in HTMX + Tailwind CSS. SQLite persistence (pure Go `modernc.org/sqlite`).

Initial technical specification is in the file StokIt_Specification.md. Update this spec as we move forward.

This app code uses Go version 1.27 or newer. Use new Go features, do not care for compatibility with older Go versions.
Go 1.27+. Use new stdlib over external libs:
- `encoding/json/v2` for all JSON. Case-sensitive, rejects duplicate members and trailing data, nil slice/map encode as `[]`/`{}`. Streaming = `json.MarshalWrite`/`json.UnmarshalRead`, not `NewEncoder`/`NewDecoder`.
- `omitzero` for bool/number fields. v2 `omitempty` no longer drops `false`/`0`.
- `uuid` (stdlib) for identifiers. `uuid.NewV7()` = unique + time-ordered. Not for secrets: session tokens stay `crypto/rand`. No use yet - all IDs are SQLite rowids; `github.com/google/uuid` stays in go.mod as an indirect dep of `mcp-go`.
- Post-quantum TLS: HTTPS listener sets an explicit TLS 1.2 floor so older clients still connect. TLS 1.3 handshakes get Go's default `X25519MLKEM768` hybrid key exchange automatically - no extra config.

Indirect deps stay pinned as-is when swapping them needs external rework.

After feature change, ensure automated tests to cover new functionality.
After feature change, ensure .md files to reflect new functionality. 

Extra tools available to agents on Windows and Linux platforms: Powershell 7.6, ripgrep 15.0. When external test/tool scripts are required, use PowerShell for cross-system compatibility.

Typical flow: review the task, if you find something unclear or inconsistent - ask me for confirmation before implementing code, implement code, update tests, run tests, document.
Maintain README.md file updated with description and functionality for user.
Maintain `openapi.yaml` for REST API changes.
Maintain `StokIt_Specification.md` to match code changes and the actual database schema — review and update it whenever schema, API, or behavior changes.
When it makes sense, keep REST API endpoints and MCP tools aligned: if a new API capability is added, add the matching MCP tool, and if a new MCP tool is added, add the matching REST API endpoint.
Treat StockIt primarily as a backend data server for external smart tools, with the bundled web UI serving as a basic manipulation console. Prefer rich, validated API and MCP capabilities over UI-only functionality.
Create and maintain tests for both REST API and MCP behavior when these surfaces change.

Always use relative paths for `apply_patch` tool calls, never absolute paths.

## Token efficiency

Think and respond like smart caveman. Cut all filler, keep technical substance.

- Drop articles (a, an, the), filler (just, really, basically, actually).
- Drop pleasantries (sure, certainly, happy to).
- No hedging. Fragments fine. Short synonyms.
- Technical terms stay exact. Code blocks unchanged.
- Pattern: [thing] [action] [reason]. [next step].
