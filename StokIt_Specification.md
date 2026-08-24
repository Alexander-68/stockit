StockIt is a high-performance, self-contained (Asset Bundling) warehouse management app built in Go with UI Layer in HTMX + Tailwind CSS. 



SQLite persistence (pure Go `modernc.org/sqlite`).

Build toolchain: Go 1.27 or newer. Standard-library features are preferred over external dependencies:

 - all REST API and MCP JSON is handled by `encoding/json/v2` (`json.MarshalWrite` / `json.UnmarshalRead`), which decodes case-sensitively and rejects duplicate object members and trailing data

 - JSON struct fields that must be omitted when zero use the `omitzero` tag option, because `omitempty` in v2 no longer drops `false` or `0`

 - the HTTPS listener sets an explicit TLS 1.2 minimum version so older clients still connect, while TLS 1.3 handshakes automatically use Go's default `X25519MLKEM768` post-quantum hybrid key exchange

 - record identifiers remain SQLite rowids; the standard-library `uuid` package is available if externally visible identifiers are ever needed, but session tokens stay `crypto/rand`

Data is stored as UTF-8.

Primary product direction:

 - StockIt should provide a basic built-in web UI for direct human data manipulation.
 - StockIt should primarily act as a validated backend data server through REST API and MCP so external smart tools can use it safely as their warehouse-data backend.

Integration-document audience: coding agents building applications on StockIt. Use root `openapi.yaml` as REST contract and runtime table catalog/schema endpoints for machine-readable table metadata. StockIt provides authenticated, role-scoped generic CRUD and MCP operations; external apps own higher-level workflows such as XLS/PDF import, stock and balance calculations, consistency checks, forecasts, and bespoke UI. This document defines product behavior and SQLite schema.

Startup hardening for service runtimes: `TMPDIR` and `SQLITE\_TMPDIR` are forced to the resolved DB directory.

Startup/runtime note:

 - on startup, the process logs the effective listen address, configured DB path, resolved DB path, working directory, and effective `TMPDIR` / `SQLITE_TMPDIR`

 - when HTTPS is enabled and the configured certificate/key files do not both exist, the process generates a self-signed certificate and key on startup

 - web UI, REST API, and MCP must all be reachable over HTTPS; MCP must also work over plain HTTP when the HTTP listener is enabled

 - interactive termination via `Ctrl-C` must gracefully stop the HTTP server and exit the process cleanly



Browser login/session notes:

&#x20; - passwords are verified against Argon2id hashes

&#x20; - browser logins use an in-memory opaque session token

&#x20; - sessions expire after 15 minutes of inactivity

&#x20; - at most 5 sessions are active per user; additional login attempts for that user are rejected until one expires or logs out

&#x20; - sessions are stored only in process memory, so all sessions are cleared on restart

&#x20; - API clients may also present the same opaque session token via `Authorization: Bearer`

&#x20; - API authentication must provide a JSON login endpoint that returns a bearer token for external clients and smart tools



\- Web dashboard with embedded assets (no internet required), including bundled favicon files (`.ico` + PNG variants)

\- Web server uses Go's standard-library `net/http` router and handlers

\- Browser write requests are protected against cross-origin submission using Go's standard-library `net/http.CrossOriginProtection`

All (REST) API endpoints require a valid session token (cookie or `Authorization: Bearer`), and a scope according to the role.

REST API and MCP rules:

 - REST API input must be validated

 - MCP tool input must be validated

 - MCP must expose application-level operations only, not raw database access

 - when it makes sense, REST API and MCP capabilities should stay aligned: adding an API capability should add the matching MCP tool, and adding an MCP tool should add the matching REST API capability

 - REST API must be documented in `openapi.yaml`

 - MCP tools are self-discoverable through MCP, but they should still be documented in user/spec docs for authentication, transport, and REST/MCP mapping clarity

Unsafe cross-origin browser requests to Web/API write endpoints are rejected with `403 Forbidden`.

Browser-based external apps must use StockIt through same-origin deployment with a reverse proxy for `/api/` and `/mcp`, or use an external-app backend that presents `Authorization: Bearer` to StockIt. StockIt does not provide CORS for direct cross-origin browser API calls. Bearer tokens must not be embedded in client-side JavaScript.

For separate trusted-LAN app servers, StockIt is central user authentication: app backend submits the user's credentials to `POST /api/auth/login`, retains returned bearer token only in its server-side session, and forwards it on StockIt calls. StockIt applies that user's role and creator-managed `usr_id`. App backend must use HTTPS, keep credentials/tokens out of logs and persistent storage, call `POST /api/auth/logout` on logout, and make user reauthentication handle StockIt's session idle expiry and process restart. Delegated SSO and app-specific permissions are future hardening.

Built-in user/role model with scoped access: admin, user, guest with passwords are stored as Argon2id.

Admin users can modify tables and manage users.

Users can modify tables, no access to users.

Guests can access tables in read-only mode.

Default draft credentials: `admin / admin`, user/user, guest/guest.

Deleting the last admin user is blocked.

MCP:

 - transport: streamable HTTP endpoint at `/mcp`

 - availability: accessible through both HTTP and HTTPS listeners

 - authentication: required before access to data or tool execution; use the same StockIt session model as web/API

 - current tool set:
   - `stockit_list_tables`
   - `stockit_describe_table`
   - `stockit_list_records` (supports `parent_id` / `parent_field` for subtables, plus `filter`, `from`, `to` column maps and a `search` string)
   - `stockit_get_record`
   - `stockit_create_record`
   - `stockit_update_record`
   - `stockit_delete_record`
   - `stockit_import_csv`
   - `stockit_submit_requisition`
   - `stockit_decide_approval`
   - `stockit_create_po_from_requisition`



UI:

One browser view - one table.

Modern, sleek, still minimalistic light UI with tight vertical and horizontal density. The app should use subtle layered surfaces, restrained gradients/background glow, refined typography, and soft shadows to feel like a polished warehouse control surface without wasting table space.

The dashboard header and active table shell should look premium but compact: glass-like/lightweight surfaces are acceptable, as long as the table remains the primary focus and the viewport is still used efficiently.

Add/Edit form is shown in a popup modal (opens on row click for edit and via a primary new-record action for new entry; modal is always dismissible).

Option to import table from CSV.

All tables use lazy loading: initial rows are sized to viewport height plus 50% buffer, then additional rows are loaded on scroll.

All tables supports column sorting: click any column header to sort ascending, click again to toggle descending, active sort column shows a little triangle at the end of the column name to show ascending or descending sorting order.

Dashboard interactions are touch-friendly: table/header taps work without mouse, tables support both horizontal and vertical swipe scrolling, table height auto-fits the current browser viewport, and controls use mobile-safe touch target sizing.

Interactive states should be clear but restrained: active navigation tabs, hovered rows, selected rows, focused form fields, and primary/destructive actions must be visually distinct without becoming visually noisy.

HTMX-like updates without full page refresh.



Key Database Schema (SQLite): 

* Users Table: usr\_id (unique), usr\_login\_name, usr\_password, usr\_role, usr\_note.
* Customers: cus\_id (unique), cus\_name\_en, cus\_name\_zh, cus\_address\_en, cus\_address\_zh, cus\_phone, cus\_ship\_address\_en, cus\_ship\_address\_zh, cus\_contact\_name, cust\_contact\_email, cus\_note, Users:usr\_id, cus\_status (active, inactive).
* Suppliers:  sup\_id (unique), sup\_code, sup\_name\_en, sup\_name\_zh, sup\_type (service,products…), sup\_contact\_name, sup\_contact\_phone, sup\_contact\_email, sup\_contact\_messanger, sup\_fax, sup\_address\_en, sup\_address\_zh, sup\_factory\_adress\_zh, sup\_website, sup\_catalogue\_url, sup\_bank\_name, sup\_bank\_account, sup\_vat\_number, sup\_certificates, sup\_note, Users:usr\_id, sup\_status.
* Locations: loc\_id (unique), loc\_name, loc\_address\_en, loc\_address\_zh, loc\_zone (storage, assembly, …), loc\_note, Users:usr\_id, loc\_status.
* Items:  itm\_id (unique), itm\_sku, itm\_model, itm\_description, itm\_value, itm\_last\_cost, itm\_avg\_cost, itm\_type (final, part, assembly), itm\_pic (BLOG), itm\_measure\_unit, itm\_note, Users:usr\_id (who created this item usr\_id), itm\_status (active, inactive).
* Bill Of Material (BOM): bom\_id(unique), bom\_doc\_number, bom\_doc\_date, Items:itm\_id, bom\_note, Users:usr\_id, bom\_status.

  * BOM components: boc\_id, BOM:bom\_id, Items:itm\_id, boc\_qty, boc\_note. (ON DELETE BOM:bom\_id CASCADE)
* Quote: qot\_id(unique), Suppliers:sup\_id, qot\_doc\_number, qot\_doc\_date, Item:itm\_id, Users:usr\_id, qot\_status (active, inactive), qot\_note.

  * Quote components: qoc\_id, Quote:qot\_id, Items:itm\_id, qot\_moq, qot\_qty, qot\_price, qot\_currency (USD, TWD, CNY, EUR), qot\_lead\_time. (ON DELETE Quote:qot\_id CASCADE)
* Purchase Order (POR): por\_id(unique), Suppliers:sup\_id, por\_doc\_number, por\_doc\_date, Items:itm\_id, por\_ship\_date, por\_paid\_date, Users:usr\_id, por\_status (draft, issued, approved, sent, confirmed, paid, prepared, shipped, delivered, received, complete, inactive), por\_note.

  * PO components: poc\_id, POR:por\_id, Items:itm\_id, poc\_qty, poc\_price, poc\_currency (USD, TWD, CNY, EUR), poc\_shipped\_date, poc\_delivered\_date, poc\_delivered\_qty, poc\_received\_date, poc\_received\_qty, poc\_iqc\_date, poc\_iqc\_package, poc\_iqc\_qty\_inspected, poc\_iqc\_qty\_accepted, poc\_iqc\_qty\_rejected, Users:usr\_id (poc\_iqc\_person). (ON DELETE POR:por\_id CASCADE)
* Purchase Requisition (PRQ): prq\_id(unique), prq\_doc\_number, prq\_date, prq\_needed\_by, prq\_department, Suppliers:sup\_id (suggested), Users:usr\_id (requester), prq\_status (draft, submitted, approved, rejected, ordered, cancelled), prq\_currency, prq\_total\_minor (priced on submission), PurchaseOrders:por\_id (set when converted), prq\_note.

  * Requisition components: prc\_id, PRQ:prq\_id, Items:itm\_id, prc\_qty, prc\_price, prc\_currency (USD, TWD, CNY, EUR), prc\_note. (ON DELETE PRQ:prq\_id CASCADE)
* Approval Rule (APR): apr\_id(unique), apr\_source\_type (purchase\_requisition), apr\_step, apr\_role (admin, user, guest), apr\_min\_amount\_minor, apr\_status (active, inactive), apr\_note. Admin-only writes. A requisition gets one approval step per active rule its total reaches, decided in `apr_step` order.
* Approval (APV): apv\_id(unique), apv\_source\_type, apv\_source\_id, apv\_step, apv\_role, apv\_status (pending, approved, rejected), Users:apv\_decided\_by, apv\_decided\_at, apv\_note. Audit trail; written only by the approval endpoints, never through the generic table API.
* Sales Order: sor\_id(unique), Customers:cus\_id, sor\_doc\_number, sor\_doc\_date, sor\_ship\_date, sor\_paid\_date, Users:usr\_id, sor\_status (confirmed, preparing, prepared, shipped, paid, complete, inactive), sor\_note.

  * Sales Order components: soc\_id, Sales Order:sor\_id, Items:itm\_id, sor\_qty, sor\_price, sor\_currency (USD, TWD, CNY, EUR), sor\_ship\_date, sor\_shipped\_date, sor\_shipped\_qty, sor\_shipped\_trackno, soc\_note. (ON DELETE Sales Order:sor\_id CASCADE)
* Manufacturing Order (MFO): mfo\_id(unique), mfo\_doc\_number, mfo\_doc\_date, mfo\_target\_date.

  * MFO components: mfc\_id, MFO:mfo\_id, Items:itm\_id, BOM:bom\_id, mfc\_qty, Sales Order:sor\_id, mfc\_qc\_date, mfc\_fqc\_date, mfc\_pack\_date, mfc\_note. (ON DELETE MFO:mfo\_id CASCADE)
* Invoice (INV): inv\_id(unique), inv\_doc\_number, inv\_doc\_date, Suppliers:sup\_id, Customers:cus\_id, Sales Order:sor\_id, inv\_shipped\_by, Users:usr\_id.

  * Invoice components: ivc\_id, Invoice:inv\_id, Items:itm\_id, ivc\_qty, ivc\_price, ivc\_currency (USD, TWD, CNY, EUR). (ON DELETE Invoice:inv\_id CASCADE)
* Adjustment (ADJ): adj\_id(unique), adj\_doc\_number, adj\_doc\_date, adj\_reason (cycle\_count, damage, loss, found, correction, write\_off, other), Users:usr\_id, adj\_note. Document of intent for stock qty changes; future stock\_moves rows will be generated from these.

  * Adjustment components: adc\_id, Adjustment:adj\_id, Items:itm\_id, Locations:loc\_id, adc\_qty (signed delta), adc\_price, adc\_currency (USD, TWD, CNY, EUR), adc\_note. (ON DELETE Adjustment:adj\_id CASCADE)
* Stock Move (STM): stm\_id(unique), stm\_doc\_number, stm\_date, Items:itm\_id, Locations:stm\_src\_loc\_id, Locations:stm\_dst\_loc\_id, stm\_qty (positive), PurchaseOrders:por\_id, SalesOrders:sor\_id, ManufacturingOrders:mfo\_id, Adjustments:adj\_id, Users:usr\_id, stm\_note. Ledger of physical movements; `stm_src_loc_id` NULL = receipt, `stm_dst_loc_id` NULL = issue, both set = transfer. Source and destination location must differ. Each FK references the originating document (at most one populated per row). Generated from finalized source documents by future logic; editable by hand for now.
* Bank Account: bnk\_id, bnk\_name, bnk\_institution, bnk\_currency, bnk\_account\_reference, bnk\_status, bnk\_note, Users:usr\_id.
* Designation Code: dsg\_id, dsg\_code, dsg\_name, dsg\_direction, dsg\_status, dsg\_note, Users:usr\_id. Seeded codes include INVENTORY, COGS, GOODS, SERVICES, SHIPPING, PAYROLL, TAX, BANK\_FEE, RENT, UTILITIES, SALES\_REVENUE, CUSTOMER\_REFUND, TRANSFER, OTHER. US GAAP does not define one universal code list; codes remain editable.
* Financial Obligation: fob\_id, fob\_type (payable, receivable), fob\_source\_type, fob\_source\_id, fob\_label, fob\_due\_date, fob\_amount\_minor (positive integer), fob\_currency, fob\_status, fob\_counterparty, fob\_note, Users:usr\_id. Multiple payable rows support PO installments; one receivable row supports single-payment SO planning. External workflows create and maintain these rows; StockIt does not automatically generate obligations, forecasts, or recurring plans.
* Bank Transaction: btx\_id, BankAccounts:bnk\_id, btx\_date, btx\_amount\_minor (signed integer; positive inflow, negative outflow), btx\_designation\_code, btx\_description, btx\_counterparty, btx\_external\_reference, FinancialObligations:fob\_id, btx\_reconciliation\_status, btx\_note, Users:usr\_id. Account balance is sum of transactions.



Notes: 

* every table contains field created\_at (auto).
* \_zh suffix means "in Chinese language".
* In web UI column names are shown "human friendly" without leading prefix: address\_en instead of cus\_address\_en.
* for status fields: Draft, Under Review, Active, Inactive, Hold, Phase-Out, Obsolete.
* root `openapi.yaml` is the maintained REST API contract.
* list endpoints filter server-side: `filter.<column>` (equality), `from.<column>` / `to.<column>` (inclusive range), `q` (substring across the table's text columns). Unknown columns are rejected.
* purchase requisition approval is the one workflow StockIt owns: `POST /api/purchase_requisitions/{id}/submit`, `POST /api/approvals/{id}/decide`, `POST /api/purchase_requisitions/{id}/purchase_order`, each mirrored by an MCP tool. Requisition totals are integer minor units, computed as `round(sum(qty * price) * 100)`.
* the bundled web UI and every app under `apps/` carry a version stamp formatted `1.0.YYMMDD`, bumped on each code change.
