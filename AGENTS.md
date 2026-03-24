StockIt is a high-performance, self-contained (Asset Bundling) warehouse management app built in Go with UI Layer in HTMX + Tailwind CSS. SQLite persistence (pure Go `modernc.org/sqlite`).

Initial technical specification is in the file StokIt_Specification.md.

This app code uses Go version 1.26 or newer. Use new Go features, do not care for compatibility with older Go versions.

Extra tools available to agents on Windows and Linux platforms: Powershell 7.6, ripgrep 15.0. When external test/tool scripts are required, use PowerShell for cross-system compatibility.

Typical flow: review the task, if you find something unclear or inconsistent - ask me for confirmation before implementing code, implement code, update tests, run tests, document.
Maintain README.md file updated with description and functionality for user.

Always use relative paths for `apply_patch` tool calls, never absolute paths.


