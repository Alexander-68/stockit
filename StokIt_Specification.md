StockIt is a high-performance, self-contained (Asset Bundling) warehouse management app built in Go with UI Layer in HTMX + Tailwind CSS. 



SQLite persistence (pure Go `modernc.org/sqlite`).

Data is stored as UTF-8.

Primary product direction:

 - StockIt should provide a basic built-in web UI for direct human data manipulation.
 - StockIt should primarily act as a validated backend data server through REST API and MCP so external smart tools can use it safely as their warehouse-data backend.

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

&#x20; - at most 5 sessions are active globally; additional login attempts are rejected until one expires or logs out

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
   - `stockit_list_records`
   - `stockit_get_record`
   - `stockit_create_record`
   - `stockit_update_record`
   - `stockit_delete_record`



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
* Purchase Order (POR): por\_id(unique), Suppliers:sup\_id, por\_doc\_number, por\_doc\_date, Items:itm\_id, por\_ship\_date, por\_paid\_date, Users:usr\_id, por\_status (issued, approved, sent, confirmed, paid, prepared, shipped, delivered, received, complete, inactive), por\_note.

  * PO components: poc\_id, POR:por\_id, Items:itm\_id, poc\_qty, poc\_price, poc\_currency (USD, TWD, CNY, EUR), poc\_shipped\_date, poc\_delivered\_date, poc\_delivered\_qty, poc\_received\_date, poc\_received\_qty, poc\_iqc\_date, poc\_iqc\_package, poc\_iqc\_qty\_inspected, poc\_iqc\_qty\_accepted, poc\_iqc\_qty\_rejected, Users:usr\_id (poc\_iqc\_person). (ON DELETE POR:por\_id CASCADE)
* Sales Order: sor\_id(unique), Customers:cus\_id, sor\_doc\_number, sor\_doc\_date, sor\_ship\_date, sor\_paid\_date, Users:usr\_id, sor\_status (confirmed, preparing, prepared, shipped, paid, complete, inactive), sor\_note.

  * Sales Order components: soc\_id, Sales Order:sor\_id, Items:itm\_id, sor\_qty, sor\_price, sor\_currency (USD, TWD, CNY, EUR), sor\_ship\_date, sor\_shipped\_date, sor\_shipped\_qty, sor\_shipped\_trackno, soc\_note. (ON DELETE Sales Order:sor\_id CASCADE)
* Manufacturing Order (MFO): mfo\_id(unique), mfo\_doc\_number, mfo\_doc\_date, mfo\_target\_date.

  * MFO components: mfc\_id, MFO:mfo\_id, Items:itm\_id, BOM:bom\_id, mfc\_qty, Sales Order:sor\_id, mfc\_qc\_date, mfc\_fqc\_date, mfc\_pack\_date, mfc\_note. (ON DELETE MFO:mfo\_id CASCADE)
* Invoice (INV): inv\_id(unique), inv\_doc\_number, inv\_doc\_date, Suppliers:sup\_id, Customers:cus\_id, Sales Order:sor\_id, inv\_shipped\_by, Users:usr\_id.

  * Invoice components: ivc\_id, Invoice:inv\_id, Items:itm\_id, ivc\_qty, ivc\_price, ivc\_currency (USD, TWD, CNY, EUR). (ON DELETE Invoice:inv\_id CASCADE)



Notes: 

* every table contains field created\_at (auto).
* \_zh suffix means "in Chinese language".
* In web UI column names are shown "human friendly" without leading prefix: address\_en instead of cus\_address\_en.
* for status fields: Draft, Under Review, Active, Hold, Phase-Out, Absolete.
* root `openapi.yaml` is the maintained REST API contract.
