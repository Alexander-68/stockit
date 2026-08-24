# Purchasing App

External app for creating and editing purchase orders and their payable payment schedules. It uses StockIt as central authentication and keeps the StockIt bearer token only in its in-memory backend session; the browser talks to a same-origin whitelisted proxy.

## Run locally

Start StockIt first:

```powershell
go run ./cmd/stockit -https-addr ""
```

Start the purchasing app in another terminal:

```powershell
go run ./apps/purchasing
```

Open `http://127.0.0.1:8091`, sign in with a StockIt user (role `user` or `admin` to write), then create or edit purchase orders.

## Scope

- Create/edit PO headers (`purchase_orders`): doc number, supplier, doc/ship/paid dates, status, note.
- Edit order lines (`po_components`): item, qty, unit price, currency. Per-currency order totals are shown. IQC/receiving fields stay editable in StockIt's own UI.
- Payment schedule: payable `financial_obligations` rows with `fob_source_type = purchase_order` and `fob_source_id` = the PO id. Amounts are StockIt integer minor units; the app does not convert line-price majors to minors.
- Print / PDF: browser print render of the PO (supplier block, lines, totals, payment schedule). Use the browser's Save as PDF.
- No import in this version; XLS import will land here later. Deleting a PO cascades its lines in StockIt but leaves obligations — remove those by hand first if needed.

The backend proxies only these tables: `suppliers` (read), `items` (read), `purchase_orders`, `po_components`, `financial_obligations` (read/write). StockIt still enforces role permissions and payload validation.

For LAN use, serve both the app and StockIt over HTTPS. When TLS terminates in a proxy in front of this app, start it with `-secure-cookies` so the session cookie still carries `Secure`. Do not put bearer tokens in browser code, logs, or persistent storage.
