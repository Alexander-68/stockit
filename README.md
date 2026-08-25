# StockIt

StockIt is a self-contained warehouse management app built in Go with a server-rendered UI using HTMX and bundled Tailwind CSS. It uses SQLite through `modernc.org/sqlite`, embeds its web assets, and starts with a prebuilt schema for warehouse master data, BOM management, and purchase-order tracking.

Building StockIt requires the Go 1.27 or newer toolchain. The REST API and MCP surfaces encode and decode JSON with the standard library's `encoding/json/v2`, so request bodies are matched case-sensitively and duplicate object members or trailing data are rejected. The HTTPS listener accepts TLS 1.2 and newer; TLS 1.3 clients negotiate Go's default `X25519MLKEM768` post-quantum hybrid key exchange.

StockIt is a secure system of record for external web apps and smart tools through REST API and MCP. The built-in web UI is intentionally limited to compact generic CRUD; workflow-specific UI and higher-level operations belong in external applications.

## Implemented Initial MVP

- SQLite schema initialization for:
  - `users` with a per-user purchase approval limit
  - `customers`
  - `suppliers`
  - `locations`
  - `items` with item value, last-cost, and average-cost fields
  - `boms`
  - `bom_components`
  - `quotes`
  - `quote_components`
  - `purchase_orders` with lifecycle status, payment tag, and priced total
  - `po_status_history` recording every purchase-order status change (when, who, note)
  - `po_components` with IQC fields
  - `sales_orders`
  - `sales_order_components`
  - `manufacturing_orders`
  - `mfo_components`
  - `invoices`
  - `invoice_components`
  - `adjustments`
  - `adjustment_components`
  - `stock_moves`
  - `bank_accounts`
  - `designation_codes`
  - `financial_obligations` for expected payable/receivable cash flows and PO installment schedules
  - `bank_transactions` for signed actual account movements and reconciliation
- Automatic default user seeding:
  - `admin / admin`
  - `user / user`
  - `guest / guest`
- Argon2id password hashing for stored credentials.
- In-memory opaque session management with:
  - 15 minute idle expiry
  - maximum 5 active sessions per user
  - cookie and `Authorization: Bearer` token support
- Dual HTTP + HTTPS serving for the web UI, REST API, and MCP endpoint.
- Startup generation of a self-signed TLS certificate and key when the configured files do not exist.
- Version stamp `1.0.YYMMDD` shown under the dashboard title; the signed-in user and role sit under the Logout button.
- Standard-library `net/http` routing and handlers.
- Cross-origin write protection using `net/http.CrossOriginProtection`.
- Defensive response headers for content type sniffing, framing, referrer policy, and CSP.
- Login attempt throttling keyed to the direct client socket address with bounded in-memory tracking.
- REST API additions for:
  - JSON login/logout
  - current principal lookup
  - table catalog discovery
  - table schema discovery
  - validated generic record list/get/create/update/delete
  - parent-filtered list (`?parent_id=<value>` for subtables)
  - server-side list filters: `?filter.<column>=` (equality), `?from.<column>=` / `?to.<column>=` (inclusive range), `?q=` (substring across text columns)
  - purchase order approval workflow (submit, decide) and recorded status/payment changes
  - multipart CSV import (`POST /api/tables/{table}/import`)
- Streamable HTTP MCP server powered by `github.com/mark3labs/mcp-go` on `/mcp`, protected by the same StockIt session authentication as the REST API.
- MCP tools aligned with REST API operations:
  - `stockit_list_tables`
  - `stockit_describe_table`
  - `stockit_list_records` (supports `parent_id` / `parent_field` for subtables, plus `filter`, `from`, `to` column maps and a `search` string)
  - `stockit_get_record`
  - `stockit_create_record`
  - `stockit_update_record`
  - `stockit_delete_record`
  - `stockit_import_csv`
  - `stockit_approve_purchase_order`
  - `stockit_submit_purchase_order`
  - `stockit_set_purchase_order_status`
- Root `openapi.yaml` maintained as the REST API contract.

Purchase order lifecycle:

- `por_status` runs through internal sign-off (`draft`, `pending_approval`, `approved`, `rejected`), vendor engagement and fulfilment (`issued`, `confirmed`, `partially_received`, `received`), accounting closure (`invoiced`, `closed`), and three exception states reachable at any point (`on_hold`, `pending_revision`, `cancelled`).
- `por_payment_status` is a separate financial tag: `unpaid`, `partially_paid`, `paid`, `refunded`. It is deliberately independent of the lifecycle, so a closed order can still be unpaid.
- Approval authority is a per-user signing limit, `users.usr_approval_limit_minor`: the largest order that user may approve, in integer minor units. An empty limit means no authority. The seeded `admin` carries a ceiling above any realistic order, so a fresh install can approve out of the box.
- Approving or rejecting reprices the order from its lines and checks that total against the deciding user's own limit. If your limit covers the order you approve it in one step from `draft`; if it does not, you submit it, it waits at `pending_approval`, and someone with a bigger limit decides. Approving your own order within your limit is allowed — that is what a signing limit means.
- `approved` and `rejected` are reachable **only** through `POST /api/purchase_orders/{id}/approve`. Writing them through the table API or `/status` is refused, so the limit cannot be walked around. Every other transition stays free.
- Every status or payment-tag change is appended to `po_status_history` with the previous status, the new status, the acting user, the timestamp and an optional note. `POST /api/purchase_orders/{id}/status` is the way to record a note with the change; a plain `PUT` on the table records the change with no note.
- `partially_received` and `received` are derived from the line receipt quantities in `po_components`. The derivation only moves an order already in the fulfilment part of the lifecycle (`issued`, `confirmed`, `partially_received`, `received`), so a draft or a closed order is never disturbed, and the derived change is recorded in the history like any other.
- `po_status_history` rows are an audit trail: they are written only by the store, never through the generic table API. One decision, one row — there is no separate approvals table.

Purchase requisitions and amount-routing approval rules were removed: purchasing drafts purchase orders directly, and a per-user signing limit replaces multi-step routing. On startup, a database written before the removal loses `purchase_requisitions`, `prq_components`, `approval_rules`, `approvals`, and the `purchase_orders.prq_id` link (the table is rebuilt without it). Purchase orders, their lines and their status history are preserved.

Databases written before the lifecycle statuses landed are migrated on startup: `sent` becomes `issued`, `prepared` and `shipped` become `confirmed`, `delivered` becomes `received`, `complete` becomes `closed`, `inactive` becomes `cancelled`, and `paid` becomes `closed` tagged `paid`. Orders with no payment tag default to `unpaid`.

Cash-planning base data:

- External workflows can create separate payable obligations for PO installments (`prepay`, `first part`, `second part`, `final`) and receivable obligations for estimated SO payments.
- Money uses integer minor units plus currency; bank transaction amounts are signed (positive inflow, negative outflow).
- Bank balances are derived from `bank_transactions`, not stored separately.
- StockIt does not generate obligations from PO/SO, calculate forecasts, or provide recurring-budget workflows. External apps implement those higher-level operations through validated CRUD.
- Embedded local assets:
  - HTMX
  - Tailwind CSS
  - app CSS/JS
  - generated favicon endpoints for `.ico`, `16x16`, and `32x32`
- HTMX dashboard with:
  - one active table view at a time
  - touch-friendly horizontal and vertical scrolling
  - column sorting
  - viewport-sized initial row loading plus scroll-based lazy loading
  - main table keyboard controls for row navigation and actions: `Up` / `Down` / `PageUp` / `PageDown` select rows, `Enter` edits, `Delete` removes, and `Insert` / `+` create
  - compact modal create/edit forms with header actions and floating field captions
  - date-aware form controls for document, shipping, delivery, payment, and receipt dates
  - modal keyboard shortcuts: `Esc` cancels, `Enter` saves, and textarea fields use `Shift+Enter` / `Ctrl+Enter` for new lines
  - parent/subtable navigation for BOM -> BOM Components, Quote -> Quote Components, Purchase Order -> PO Components, and Sales Order -> Sales Order Components, with parent row selection opening the filtered child list
  - selected-parent context shown above subtable lists, with child create/edit forms inheriting and hiding the parent foreign key
  - compact dropdown selectors for status fields and foreign-key fields using key reference columns
  - creator-managed `usr_id` fields that are set automatically from the logged-in user instead of being selectable in forms
  - CSV import per writable table
  - modern light glass-style visual system with compact premium surfaces and restrained motion
  - compact single-line textarea inputs that auto-grow as additional lines are entered
- Built-in role rules:
  - `admin`: manage all tables and users
  - `user`: manage non-user tables
  - `guest`: read-only access to non-user tables
- Guard against deleting the last admin account.

## Notes

- SQLite temp directories are forced to the resolved database directory through `TMPDIR` and `SQLITE_TMPDIR` during startup.
- User password hashes are never returned by the generic UI/API table readers.
- Session cookies are `HttpOnly` and `SameSite=Strict`, and are marked `Secure` only for real TLS requests.
- The default HTTPS certificate is self-signed for local/private-network use. Replace it with your own certificate/key pair for production-like deployments.
- `X-Forwarded-For` and `X-Forwarded-Proto` are not trusted by default; add explicit trusted-proxy handling before deploying behind a reverse proxy that terminates TLS.
- The current UI direction is intentionally sleek and modern while preserving dense table-first workflows; it uses subtle gradients, layered light surfaces, and stronger active/focus states instead of the original bare prototype styling.
- Status fields currently use a combined option set from the specification text:
  - `Draft`
  - `Under Review`
  - `Active`
  - `Inactive`
  - `Hold`
  - `Phase-Out`
  - `Obsolete`
- Databases created with earlier builds that stored the misspelled `Absolete`
  value are automatically rewritten to `Obsolete` on startup.

## Run

```powershell
go run ./cmd/stockit -addr 127.0.0.1:8080 -https-addr 127.0.0.1:8443 -db .\data\stockit.db
```

Then open either `http://127.0.0.1:8080` or `https://127.0.0.1:8443`.

Useful flags:

- `-https-addr ""` disables the HTTPS listener.
- `-tls-cert` and `-tls-key` override the certificate/key paths. Missing files are generated automatically on startup.

Startup logs print the effective listen address, requested and resolved DB path, working directory, and the runtime `TMPDIR` / `SQLITE_TMPDIR` values.

## REST API And MCP

`openapi.yaml` describes the REST API. The same session model is used for web UI, REST API, and MCP:

- Browser clients can use the `stockit_session` cookie.
- API and MCP clients can use `Authorization: Bearer <token>`.
- `POST /api/auth/login` returns both a JSON bearer token and a session cookie.

Current REST endpoints:

- `POST /api/auth/login`
- `POST /api/auth/logout`
- `GET /api/me`
- `GET /api/tables`
- `GET /api/tables/{table}/schema`
- `GET /api/tables/{table}` (supports `?parent_id=<value>` and optional `?parent_field=<column>` for subtables; `?filter.<column>=`, `?from.<column>=`, `?to.<column>=`, `?q=`)
- `GET /api/tables/{table}/{id}`
- `POST /api/tables/{table}`
- `PUT /api/tables/{table}/{id}`
- `DELETE /api/tables/{table}/{id}`
- `POST /api/tables/{table}/import` (multipart `csv_file` upload)
- `POST /api/purchase_orders/{id}/submit`
- `POST /api/purchase_orders/{id}/status`
- `POST /api/purchase_orders/{id}/approve`

Current MCP transport:

- Streamable HTTP endpoint at `/mcp`
- Available over both HTTP and HTTPS listeners
- Requires StockIt authentication before `initialize` and subsequent MCP calls

MCP tools are self-discoverable through `tools/list`, but they are still documented here and in the specification because human-facing docs are needed for auth flow, REST/MCP mapping, and expected operator workflows.

### External web apps

External apps may be a static `index.html` with any basic server, or a Python/Go/Node application. Deploy browser apps behind the same origin as StockIt and proxy `/api/` and `/mcp` to StockIt; then use relative API URLs and the session cookie. A different browser origin cannot call StockIt directly because StockIt does not enable CORS and rejects unsafe cross-origin writes. Do not embed bearer tokens in client-side JavaScript; use a same-origin session or an external-app backend that holds its bearer token.

For separate app servers during LAN trials, use StockIt as central user authentication:

1. Browser submits credentials only to its app backend over HTTPS.
2. App backend calls `POST /api/auth/login` over HTTPS and receives that user's bearer token.
3. Backend keeps token only in its server-side session, mapped to its own `HttpOnly`, `Secure`, `SameSite=Strict` browser cookie.
4. Backend calls StockIt with `Authorization: Bearer <token>`; StockIt applies user role and sets creator-managed `usr_id` fields to that real user.
5. App logout calls `POST /api/auth/logout`; user must reauthenticate after StockIt's 15-minute idle expiry or restart.

This is trusted-LAN trial flow. Never log credentials/tokens or persist user tokens. Replace it with delegated SSO and app-specific permissions before untrusted or internet-facing deployment.

### Connecting Claude Code to the StockIt MCP server

Claude Code (the CLI) can talk to the StockIt MCP endpoint over HTTP using a bearer token obtained from `/api/auth/login`.

1. Obtain a bearer token (PowerShell):
   
   ```powershell
   Invoke-RestMethod -Uri http://127.0.0.1:8080/api/auth/login `
     -Method Post -ContentType "application/json" `
     -Body '{"login_name":"admin","password":"admin"}'
   ```
   
   The response contains a `token` field, for example `abc123...`.

2. Register the MCP server in Claude Code, passing the token as an `Authorization` header:
   
   ```powershell
   claude mcp add --transport http stockit http://127.0.0.1:8080/mcp `
     --header "Authorization: Bearer <token>"
   ```

3. Inside Claude Code, run `/mcp` to confirm the `stockit` server is connected. The StockIt tools become available as `mcp__stockit__stockit_list_tables`, `mcp__stockit__stockit_get_record`, etc. Write tools (`create`/`update`/`delete`) only appear if the authenticated role has writable tables.

Notes:

- Bearer tokens expire when the session idles out (`session_idle_timeout_seconds` in the login response). If `/mcp` later reports an auth failure, repeat step 1 and re-add the server with `claude mcp remove stockit` followed by step 2.
- The same flow works against the HTTPS listener (`https://127.0.0.1:8443/mcp`); with a self-signed certificate, set `NODE_TLS_REJECT_UNAUTHORIZED=0` in the environment before launching Claude Code, or use the plain HTTP listener for local development.
- Remote hosts such as claude.ai cannot reach `127.0.0.1` directly and require a public HTTPS tunnel (e.g. `cloudflared tunnel --url http://127.0.0.1:8080`).

## Project Layout

- [`cmd/stockit/main.go`](/C:/Alex/StockIt/cmd/stockit/main.go): entry point
- [`internal/app/app.go`](/C:/Alex/StockIt/internal/app/app.go): server, routes, handlers, API
- [`internal/app/mcp.go`](/C:/Alex/StockIt/internal/app/mcp.go): MCP transport and tool registration
- [`internal/app/tls.go`](/C:/Alex/StockIt/internal/app/tls.go): self-signed TLS certificate generation
- [`internal/auth/password.go`](/C:/Alex/StockIt/internal/auth/password.go): Argon2id hashing and verification
- [`internal/auth/session.go`](/C:/Alex/StockIt/internal/auth/session.go): in-memory session manager
- [`internal/store/sqlite.go`](/C:/Alex/StockIt/internal/store/sqlite.go): SQLite initialization and data access
- [`internal/store/metadata.go`](/C:/Alex/StockIt/internal/store/metadata.go): table metadata and permissions
- [`openapi.yaml`](/C:/Alex/StockIt/openapi.yaml): REST API contract
- [`TESTING.md`](/C:/Alex/StockIt/TESTING.md): test commands and review seed dataset
- [`apps/cashflow`](/C:/Alex/StockIt/apps/cashflow): first external-app example
- [`internal/web/templates`](/C:/Alex/StockIt/internal/web/templates): HTML templates
- [`internal/web/assets`](/C:/Alex/StockIt/internal/web/assets): bundled frontend assets

