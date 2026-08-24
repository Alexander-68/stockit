# External Apps

StockIt is system of record. Apps here own workflow UI and call StockIt REST API through same-origin proxy or app backend.

Read [StockIt specification](../StokIt_Specification.md) and [REST API contract](../openapi.yaml) before building an app. Discover table fields and permissions at runtime through `/api/tables` and `/api/tables/{table}/schema`.

Start with these independent apps:

1. `cashflow` — read bank transactions and open financial obligations; show per-currency daily/weekly/monthly projected balance.
2. `purchasing` — create/import purchase orders and their payable installment obligations. XLS parsing stays here.
3. `sales` — create/import sales orders and estimated receivable obligations. PDF extraction stays here.
4. `banking` — import bank CSV, create bank transactions, link them to obligations, flag unreconciled rows.

Build `cashflow` first. It proves authentication, metadata discovery, reads, currency handling, and projected-balance calculations without creating new StockIt endpoints.

Do not share browser bearer tokens. Deploy static browser apps behind same-origin `/api/` and `/mcp` proxy, or keep bearer tokens in app backend.

For separate trusted-LAN app servers, use StockIt API login as central authentication: backend exchanges user credentials for bearer token over HTTPS, retains token only in server-side app session, and forwards it on API calls. This preserves StockIt user attribution. See root README for full trial-flow limits.
