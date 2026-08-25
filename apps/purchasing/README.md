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

Version: `1.0.260825` (shown under the app title).

## Scope

- Browse POs with a filter bar: doc-date range (defaults to the last 12 months), supplier, status, and a doc-number/note search. All four filter server-side through StockIt's list filters, so the browser only holds the window it asked for. Click a column title to sort, click again to reverse.
- Order total per PO is shown in the list, summed per currency from that order's lines.
- Create/edit PO headers (`purchase_orders`): doc number, supplier, doc/ship/paid dates, status, note. New POs start as `draft`.
- Copy PO: duplicates the header and all lines into a new draft order under a document number you choose.
- Goods receipt: each order line carries received qty and received date, with outstanding qty shown per line and a received/ordered unit count for the order. "Receive all" fills a line from its ordered qty.
- Supplier management: full CRUD over `suppliers`, plus each supplier's purchase history and total ordered value per currency.
- Supplier price list: active `quotes` for the PO's supplier fill a new line's price and currency from the newest quote for that item, showing MOQ and lead time. A price more than 0.5% away from the quoted price raises a visible warning; it never blocks the order.
- Edit order lines (`po_components`): item, qty, unit price, currency. Per-currency order totals are shown. IQC/receiving fields stay editable in StockIt's own UI.
- Requisitions tab: draft a purchase requisition (`purchase_requisitions` + `prq_components`), submit it for approval, decide the steps you are the approver for, and convert an approved requisition into a draft purchase order. Header and lines lock once the requisition leaves draft; the approvals panel shows every step with its role, decision, decider and note, and only offers Approve/Reject on the first pending step whose role matches your StockIt role. Leaving the purchase order document number empty reuses the requisition document number.
- Payment schedule: payable `financial_obligations` rows with `fob_source_type = purchase_order` and `fob_source_id` = the PO id. Amounts are StockIt integer minor units; the app does not convert line-price majors to minors.
- Print: browser print render of the PO (supplier block, lines, totals, payment schedule). Use the browser's Save as PDF.
- No import in this version; XLS import will land here later. Deleting a PO cascades its lines in StockIt but leaves obligations — remove those by hand first if needed.

The backend proxies only these tables: `items`, `quotes`, `quote_components`, `approvals` (read), `suppliers`, `purchase_orders`, `po_components`, `purchase_requisitions`, `prq_components`, `financial_obligations` (read/write). It also forwards three StockIt approval actions: `POST /api/purchase_requisitions/{id}/submit`, `POST /api/purchase_requisitions/{id}/purchase_order`, and `POST /api/approvals/{id}/decide`. StockIt still enforces role permissions and payload validation.

For LAN use, serve both the app and StockIt over HTTPS. When TLS terminates in a proxy in front of this app, start it with `-secure-cookies` so the session cookie still carries `Secure`. Do not put bearer tokens in browser code, logs, or persistent storage.
