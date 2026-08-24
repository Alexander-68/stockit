# Testing

Run full automated suite:

```powershell
go test ./...
```

## Review seed dataset

`TestSeedReviewDataset` builds realistic cross-linked SQLite data for manual review.

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db
```

Default output: `testdata/review-db/TestSeedReviewDataset.db`. Kept runs retain SQLite `.db`, `-wal`, and `-shm` files; test output prints path.

Use different output directory:

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db -stockit-db-dir .\testdata\review-db
```

Populate database opened by app:

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db -stockit-db-path .\data\stockit.db
```

Open kept default dataset:

```powershell
go run ./cmd/stockit -db .\testdata\review-db\TestSeedReviewDataset.db
```

### Dataset convention

- 3 rows per top-level table: customers, suppliers, locations, boms, purchase_orders, quotes, sales_orders, manufacturing_orders, invoices, adjustments, stock_moves. `users` uses default `admin`, `user`, and `guest` accounts.
- 9 rows per subtable: `bom_components`, `po_components`, `quote_components`, `sales_order_components`, `mfo_components`, `invoice_components`, `adjustment_components` (3 parents × 3 lines).
- 3 `stock_moves`: PO receipt, SO issue, Adjustment transfer.
- 15 `items`: 3 finals, 9 parts, 3 assemblies. Unique parts avoid component-line collisions.

When adding table, extend seed: 3 top-level rows or 9 subtable rows, then add matching verification row in `TestSeedReviewDataset`.
