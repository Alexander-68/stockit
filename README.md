# StockIt

StockIt is a self-contained warehouse management app built in Go with a server-rendered UI using HTMX and bundled Tailwind CSS. It uses SQLite through `modernc.org/sqlite`, embeds its web assets, and starts with a prebuilt schema for warehouse master data and BOM management.

## Implemented Initial MVP

- SQLite schema initialization for:
  - `users`
  - `customers`
  - `suppliers`
  - `locations`
  - `items`
  - `boms`
  - `bom_components`
- Automatic default user seeding:
  - `admin / admin`
  - `user / user`
  - `guest / guest`
- Argon2id password hashing for stored credentials.
- In-memory opaque session management with:
  - 15 minute idle expiry
  - maximum 5 active sessions globally
  - cookie and `Authorization: Bearer` token support
- Standard-library `net/http` routing and handlers.
- Cross-origin write protection using `net/http.CrossOriginProtection`.
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
  - modal create/edit forms
  - CSV import per writable table
- Built-in role rules:
  - `admin`: manage all tables and users
  - `user`: manage non-user tables
  - `guest`: read-only access to non-user tables
- Guard against deleting the last admin account.
- Minimal JSON API for table list/get/create/update/delete plus `/api/me`.
- Extensive integration and store tests covering login, roles, CSRF protection, CRUD, CSV import, sorting, hidden password hashes, and BOM cascade behavior.

## Notes

- SQLite temp directories are forced to the resolved database directory through `TMPDIR` and `SQLITE_TMPDIR` during startup.
- User password hashes are never returned by the generic UI/API table readers.
- Status fields currently use a combined option set from the specification text:
  - `Draft`
  - `Under Review`
  - `Active`
  - `Inactive`
  - `Hold`
  - `Phase-Out`
  - `Absolete`

## Run

```powershell
go run ./cmd/stockit -addr 127.0.0.1:8080 -db .\data\stockit.db
```

Then open `http://127.0.0.1:8080`.

Startup logs print the effective listen address, requested and resolved DB path, working directory, and the runtime `TMPDIR` / `SQLITE_TMPDIR` values.

## Test

```powershell
go test ./...
```

To keep populated test databases after the run for manual review:

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db
```

This writes the review database to [`testdata/review-db/TestSeedReviewDataset.db`](/C:/Alex/StockIt/testdata/review-db/TestSeedReviewDataset.db).

Optional custom output directory for kept databases:

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db -stockit-db-dir .\testdata\review-db
```

To populate the exact database file that the app will open by default:

```powershell
go test ./internal/app -run TestSeedReviewDataset -v -args -stockit-keep-db -stockit-db-path .\data\stockit.db
```

When enabled, the tests log the database path and keep the SQLite `.db`, `-wal`, and `-shm` files instead of using an auto-cleaned temp directory.

If you keep the review data in a non-default path, start the app with the same database path:

```powershell
go run ./cmd/stockit -db .\testdata\review-db\TestSeedReviewDataset.db
```

## Project Layout

- [`cmd/stockit/main.go`](/C:/Alex/StockIt/cmd/stockit/main.go): entry point
- [`internal/app/app.go`](/C:/Alex/StockIt/internal/app/app.go): server, routes, handlers, API
- [`internal/auth/password.go`](/C:/Alex/StockIt/internal/auth/password.go): Argon2id hashing and verification
- [`internal/auth/session.go`](/C:/Alex/StockIt/internal/auth/session.go): in-memory session manager
- [`internal/store/sqlite.go`](/C:/Alex/StockIt/internal/store/sqlite.go): SQLite initialization and data access
- [`internal/store/metadata.go`](/C:/Alex/StockIt/internal/store/metadata.go): table metadata and permissions
- [`internal/web/templates`](/C:/Alex/StockIt/internal/web/templates): HTML templates
- [`internal/web/assets`](/C:/Alex/StockIt/internal/web/assets): bundled frontend assets
