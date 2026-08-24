# Cashflow App

Read-only external app for current bank balances and open payable/receivable obligations. It uses StockIt as central authentication and keeps StockIt bearer token only in its in-memory backend session.

## Run locally

Start StockIt first:

```powershell
go run ./cmd/stockit -https-addr ""
```

Start cashflow app in another terminal:

```powershell
go run ./apps/cashflow
```

Open `http://127.0.0.1:8090`, sign in with a StockIt user, then view bank balances and scheduled obligations. Amounts stay in StockIt integer minor units; app does not assume currency decimal scale.

## Scope

- Default report: today through 90 days ahead. Both dates are editable.
- Balance: sum of `bank_transactions` per `bank_account` dated on or before as-of date.
- Forecast: recorded balance plus open `financial_obligations`, grouped by currency and due date. `payable` subtracts; `receivable` adds. Past-due rows form an overdue bucket at as-of date; rows after forecast-through are excluded. `paid` and `cancelled` rows are excluded.
- Hover forecast row for included obligation details. Use Print for browser print or PDF output.
- This app reads only. Create/import/reconciliation workflows stay in banking, purchasing, and sales apps.

For LAN use, serve both app and StockIt over HTTPS. Do not put bearer tokens in browser code, logs, or persistent storage.
