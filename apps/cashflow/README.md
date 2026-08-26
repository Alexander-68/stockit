# Cashflow App

External app for current bank balances and open payable/receivable obligations, plus entry of recurring commitments such as rent and payroll. It uses StockIt as central authentication and keeps StockIt bearer token only in its in-memory backend session.


Version: `1.0.260826` (shown under the app title).

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
- Forecast: per-currency opening balance plus open `financial_obligations`, grouped by currency and due date. The opening balance per currency is listed above the forecast; a currency with obligations but no bank account opens at zero and is marked as such. `payable` subtracts; `receivable` adds. Past-due rows form an overdue bucket at as-of date; rows after forecast-through are excluded. `paid` and `cancelled` rows are excluded.
- Obligation details show the designation code with its name where one is set.
- Hover a forecast row (or tap it on touch devices) for the included obligation details. Use Print for browser print or PDF output.
- Recurring payments: the form writes one planned `financial_obligation` per installment (weekly, monthly, quarterly or yearly, up to 60). A month-end start date stays at month end. The optional designation code comes from StockIt's `designation_codes`, offered as `CODE - Name` and stored as the bare code; only Active codes are offered, but an obligation holding a retired code still displays it. There is no recurrence record, so a later change (a rent increase, a cancelled month) is an ordinary edit of the affected rows. Installments are created one by one without a transaction; the response reports how many landed if StockIt rejects one mid-run.
- Apart from that form, this app reads only. Import and reconciliation workflows stay in banking, purchasing, and sales apps.

For LAN use, serve both app and StockIt over HTTPS. When TLS terminates in a proxy in front of this app, start it with `-secure-cookies` so the session cookie still carries `Secure`. Do not put bearer tokens in browser code, logs, or persistent storage.
