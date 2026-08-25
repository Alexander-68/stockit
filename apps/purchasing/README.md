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

Version: `1.0.260826` (shown under the app title).

## Scope

- Browse POs with a filter bar: doc-date range (defaults to the last 12 months), supplier, status, payment tag, and a doc-number/note search. All of them filter server-side through StockIt's list filters, so the browser only holds the window it asked for. Click a column title to sort, click again to reverse.
- Order total per PO is shown in the list, summed per currency from that order's lines.
- Create/edit PO headers (`purchase_orders`): doc number, supplier, doc/ship/paid dates, currency, note. New POs start as `draft` / `unpaid`. The header form no longer carries the status: status and payment tag move through the Status panel so every change is recorded with a note.
- Duplicate: creates a new draft order from the open one, for re-ordering the same basket. It copies the supplier, the currency, the note and every line (item, qty, unit price, currency), and starts `unpaid`; it does not copy receipts, ship/paid dates or the payment schedule, which belong to the original order. The copy is created in StockIt immediately as a `draft` with the document number `<original>-COPY`, then opens with that number selected so you can rename it and save. Delete it if you created it by mistake.
- Status, approvals & payment: the full lifecycle (`draft`, `pending_approval`, `approved`, `rejected`, `issued`, `confirmed`, `partially_received`, `received`, `invoiced`, `closed`, `on_hold`, `pending_revision`, `cancelled`) plus an independent payment tag (`unpaid`, `partially_paid`, `paid`, `refunded`). "Apply status change" posts status, payment tag and note to StockIt. Approval runs off your own signing limit (`usr_approval_limit_minor` in StockIt's Users table): if it covers the order total the button reads **Approve** and a Reject button appears beside it, so a decision is one click from draft; if it does not, the button reads **Submit for approval**, a line explains why, and the order waits at `pending_approval` for someone with more authority. `approved` and `rejected` are not offered in the status dropdown — StockIt only accepts them through the approve endpoint. The Status history table below lists every recorded change: when, from, to, payment tag, who (by login name) and the note.
- Goods receipt: each order line carries received qty and received date, with outstanding qty shown per line and a received/ordered unit count for the order. "Receive all" fills a line from its ordered qty. StockIt derives `partially_received` / `received` from these quantities for orders already in the fulfilment part of the lifecycle, and records the derived change in the status history.
- Supplier management: full CRUD over `suppliers`, plus each supplier's purchase history and total ordered value per currency.
- Supplier price list: active `quotes` for the PO's supplier fill a new line's price and currency from the newest quote for that item, showing MOQ and lead time. A price more than 0.5% away from the quoted price raises a visible warning; it never blocks the order.
- Edit order lines (`po_components`): item, qty, unit price, currency. Per-currency order totals are shown. IQC/receiving fields stay editable in StockIt's own UI.
- Payment schedule: payable `financial_obligations` rows with `fob_source_type = purchase_order` and `fob_source_id` = the PO id. Amounts are StockIt integer minor units; the app does not convert line-price majors to minors.
- Print: browser print render of the PO (supplier block, lines, totals, payment schedule). The header shows the current status with whoever set it ("Status: Approved by alex") and an "Issued by" line naming whoever moved the order to `issued`, falling back to whoever drafted it. Use the browser's Save as PDF.
- No import in this version; XLS import will land here later. Deleting a PO cascades its lines in StockIt but leaves obligations — remove those by hand first if needed.

The backend proxies only these tables: `items`, `quotes`, `quote_components`, `po_status_history` (read), `suppliers`, `purchase_orders`, `po_components`, `financial_obligations` (read/write). It also forwards three StockIt workflow actions: `POST /api/purchase_orders/{id}/submit`, `POST /api/purchase_orders/{id}/approve`, and `POST /api/purchase_orders/{id}/status`. Your signing limit is read once at login from StockIt's `/api/me`, and user login names come from `GET /api/users/names`. StockIt still enforces role permissions and payload validation.

For LAN use, serve both the app and StockIt over HTTPS. When TLS terminates in a proxy in front of this app, start it with `-secure-cookies` so the session cookie still carries `Secure`. Do not put bearer tokens in browser code, logs, or persistent storage.
