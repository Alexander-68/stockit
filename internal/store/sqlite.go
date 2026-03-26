package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"stockit/internal/auth"
)

type Store struct {
	db     *sql.DB
	tables map[string]TableDef
}

type ListOptions struct {
	Sort   string
	Desc   bool
	Limit  int
	Offset int
	Filter map[string]any
}

type ListResult struct {
	Rows    []map[string]any
	HasMore bool
}

type Option struct {
	Value string
	Label string
}

type UserRecord struct {
	ID           int64
	LoginName    string
	PasswordHash string
	Role         string
}

func Open(ctx context.Context, dbPath string) (*Store, error) {
	resolvedPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}

	dbDir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{
		db:     db,
		tables: AllTables(),
	}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Table(name string) (TableDef, bool) {
	table, ok := s.tables[name]
	return table, ok
}

func (s *Store) TablesForRole(role string) []TableDef {
	tables := make([]TableDef, 0, len(s.tables))
	for _, table := range s.tables {
		if table.CanRead(role) && !table.IsSubtable() {
			tables = append(tables, table)
		}
	}
	slices.SortFunc(tables, func(a, b TableDef) int {
		return strings.Compare(a.Label, b.Label)
	})
	return tables
}

func (s *Store) AuthenticateUser(ctx context.Context, loginName string) (UserRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT usr_id, usr_login_name, usr_password, usr_role FROM users WHERE usr_login_name = ?`,
		loginName,
	)

	var user UserRecord
	if err := row.Scan(&user.ID, &user.LoginName, &user.PasswordHash, &user.Role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, err
		}
		return UserRecord{}, fmt.Errorf("scan user: %w", err)
	}
	return user, nil
}

func (s *Store) List(ctx context.Context, tableName string, opts ListOptions) (ListResult, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return ListResult{}, fmt.Errorf("unknown table %q", tableName)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	sortColumn := table.SortColumn(opts.Sort)
	direction := "ASC"
	if opts.Desc {
		direction = "DESC"
	}

	filterColumns := make([]string, 0, len(opts.Filter))
	for column := range opts.Filter {
		filterColumns = append(filterColumns, column)
	}
	slices.Sort(filterColumns)

	whereClauses := make([]string, 0, len(opts.Filter))
	args := make([]any, 0, len(opts.Filter)+2)
	for _, column := range filterColumns {
		value := opts.Filter[column]
		field, ok := table.Field(column)
		if !ok {
			return ListResult{}, fmt.Errorf("unknown filter column %q for table %q", column, tableName)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("%s = ?", quoteIdent(field.Column)))
		args = append(args, value)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s%s ORDER BY %s %s LIMIT ? OFFSET ?`,
		joinQuoted(selectColumns(table)),
		quoteIdent(table.Name),
		whereSQL,
		quoteIdent(sortColumn),
		direction,
	)
	args = append(args, limit+1, opts.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("list rows: %w", err)
	}
	defer rows.Close()

	records, err := scanRows(rows)
	if err != nil {
		return ListResult{}, err
	}

	result := ListResult{Rows: records}
	if len(records) > limit {
		result.HasMore = true
		result.Rows = records[:limit]
	}
	return result, nil
}

func (s *Store) Get(ctx context.Context, tableName string, id string) (map[string]any, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return nil, fmt.Errorf("unknown table %q", tableName)
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s = ?`,
		joinQuoted(selectColumns(table)),
		quoteIdent(table.Name),
		quoteIdent(table.PrimaryKey),
	)
	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("get row: %w", err)
	}
	defer rows.Close()

	records, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, sql.ErrNoRows
	}
	return records[0], nil
}

func (s *Store) Insert(ctx context.Context, tableName string, values map[string]any) (int64, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return 0, fmt.Errorf("unknown table %q", tableName)
	}

	columns := table.InsertableColumns(values)
	if len(columns) == 0 {
		query := fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, quoteIdent(table.Name))
		result, err := s.db.ExecContext(ctx, query)
		if err != nil {
			return 0, fmt.Errorf("insert default row: %w", err)
		}
		return result.LastInsertId()
	}

	args := make([]any, 0, len(columns))
	placeholders := make([]string, 0, len(columns))
	for _, column := range columns {
		args = append(args, values[column])
		placeholders = append(placeholders, "?")
	}

	query := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s)`,
		quoteIdent(table.Name),
		joinQuoted(columns),
		strings.Join(placeholders, ", "),
	)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("insert row: %w", err)
	}
	return result.LastInsertId()
}

func (s *Store) Update(ctx context.Context, tableName string, id string, values map[string]any) error {
	table, ok := s.Table(tableName)
	if !ok {
		return fmt.Errorf("unknown table %q", tableName)
	}

	columns := table.UpdatableColumns(values)
	if len(columns) == 0 {
		return nil
	}

	assignments := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)+1)
	for _, column := range columns {
		assignments = append(assignments, fmt.Sprintf("%s = ?", quoteIdent(column)))
		args = append(args, values[column])
	}
	args = append(args, id)

	query := fmt.Sprintf(
		`UPDATE %s SET %s WHERE %s = ?`,
		quoteIdent(table.Name),
		strings.Join(assignments, ", "),
		quoteIdent(table.PrimaryKey),
	)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update row: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, tableName string, id string) error {
	table, ok := s.Table(tableName)
	if !ok {
		return fmt.Errorf("unknown table %q", tableName)
	}

	query := fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, quoteIdent(table.Name), quoteIdent(table.PrimaryKey))
	if _, err := s.db.ExecContext(ctx, query, id); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	return nil
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE usr_role = 'admin'`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return count, nil
}

func (s *Store) ReferenceOptions(ctx context.Context, tableName string) ([]Option, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return nil, fmt.Errorf("unknown reference table %q", tableName)
	}

	columns := table.ReferenceColumns()
	if len(columns) == 0 {
		columns = []string{table.PrimaryKey}
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s ORDER BY %s ASC`,
		joinQuoted(columns),
		quoteIdent(table.Name),
		quoteIdent(table.TitleColumn),
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list reference options: %w", err)
	}
	defer rows.Close()

	options := []Option{{Value: "", Label: ""}}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for index := range values {
			dest[index] = &values[index]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan reference option: %w", err)
		}

		parts := make([]string, 0, len(columns))
		for index, column := range columns {
			field, ok := table.Field(column)
			if !ok {
				continue
			}
			label := DisplayValue(field, normalizeValue(values[index]))
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			parts = append(parts, label)
		}

		value := fmt.Sprint(normalizeValue(values[0]))
		label := strings.Join(parts, " | ")
		if label == "" {
			label = value
		}

		options = append(options, Option{
			Value: value,
			Label: label,
		})
	}
	return options, rows.Err()
}

func (s *Store) ImportCSV(ctx context.Context, tableName string, reader io.Reader, transform func(Field, string) (any, error)) (int, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return 0, fmt.Errorf("unknown table %q", tableName)
	}

	csvReader := csv.NewReader(reader)
	csvReader.TrimLeadingSpace = true

	headers, err := csvReader.Read()
	if err != nil {
		return 0, fmt.Errorf("read csv header: %w", err)
	}

	headerMap := make(map[int]Field)
	for index, header := range headers {
		normalized := NormalizeCSVHeader(header)
		for _, field := range table.EditableFields() {
			if NormalizeCSVHeader(field.Column) == normalized || NormalizeCSVHeader(field.Label) == normalized {
				headerMap[index] = field
				break
			}
		}
	}

	inserted := 0
	for {
		record, err := csvReader.Read()
		if errors.Is(err, io.EOF) {
			return inserted, nil
		}
		if err != nil {
			return inserted, fmt.Errorf("read csv row: %w", err)
		}

		values := make(map[string]any)
		for index, rawValue := range record {
			field, ok := headerMap[index]
			if !ok {
				continue
			}
			parsedValue, err := transform(field, rawValue)
			if err != nil {
				return inserted, fmt.Errorf("parse csv field %s: %w", field.Column, err)
			}
			if parsedValue != nil {
				values[field.Column] = parsedValue
			}
		}

		if _, err := s.Insert(ctx, tableName, values); err != nil {
			return inserted, err
		}
		inserted++
	}
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`CREATE TABLE IF NOT EXISTS users (
			usr_id INTEGER PRIMARY KEY AUTOINCREMENT,
			usr_login_name TEXT NOT NULL UNIQUE,
			usr_password TEXT NOT NULL,
			usr_role TEXT NOT NULL,
			usr_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS customers (
			cus_id INTEGER PRIMARY KEY AUTOINCREMENT,
			cus_name_en TEXT NOT NULL,
			cus_name_zh TEXT,
			cus_address_en TEXT,
			cus_address_zh TEXT,
			cus_phone TEXT,
			cus_ship_address_en TEXT,
			cus_ship_address_zh TEXT,
			cus_contact_name TEXT,
			cust_contact_email TEXT,
			cus_note TEXT,
			usr_id INTEGER,
			cus_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS suppliers (
			sup_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_code TEXT,
			sup_name_en TEXT NOT NULL,
			sup_name_zh TEXT,
			sup_type TEXT,
			sup_contact_name TEXT,
			sup_contact_phone TEXT,
			sup_contact_email TEXT,
			sup_contact_messanger TEXT,
			sup_fax TEXT,
			sup_address_en TEXT,
			sup_address_zh TEXT,
			sup_factory_adress_zh TEXT,
			sup_website TEXT,
			sup_catalogue_url TEXT,
			sup_bank_name TEXT,
			sup_bank_account TEXT,
			sup_vat_number TEXT,
			sup_certificates TEXT,
			sup_note TEXT,
			usr_id INTEGER,
			sup_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS locations (
			loc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			loc_name TEXT NOT NULL,
			loc_address_en TEXT,
			loc_address_zh TEXT,
			loc_zone TEXT,
			loc_note TEXT,
			usr_id INTEGER,
			loc_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS items (
			itm_id INTEGER PRIMARY KEY AUTOINCREMENT,
			itm_sku TEXT NOT NULL,
			itm_model TEXT,
			itm_description TEXT,
			itm_value REAL,
			itm_type TEXT,
			itm_pic BLOB,
			itm_measure_unit TEXT,
			itm_note TEXT,
			usr_id INTEGER,
			itm_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS boms (
			bom_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bom_doc_number TEXT NOT NULL,
			bom_doc_date TEXT,
			itm_id INTEGER,
			bom_note TEXT,
			usr_id INTEGER,
			bom_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS bom_components (
			boc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bom_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			boc_qty REAL,
			boc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (bom_id) REFERENCES boms (bom_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS purchase_orders (
			por_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			por_doc_number TEXT NOT NULL,
			por_doc_date TEXT,
			itm_id INTEGER,
			por_ship_date TEXT,
			por_paid_date TEXT,
			usr_id INTEGER,
			por_status TEXT,
			por_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS quotes (
			qot_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			qot_doc_number TEXT NOT NULL,
			qot_doc_date TEXT,
			itm_id INTEGER,
			usr_id INTEGER,
			qot_status TEXT,
			qot_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS quote_components (
			qoc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			qot_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			qot_moq REAL,
			qot_qty REAL,
			qot_price REAL,
			qot_currency TEXT,
			qot_lead_time TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (qot_id) REFERENCES quotes (qot_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS po_components (
			poc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			por_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			poc_qty REAL,
			poc_price REAL,
			poc_currency TEXT,
			poc_shipped_date TEXT,
			poc_delivered_date TEXT,
			poc_delivered_qty REAL,
			poc_received_date TEXT,
			poc_received_qty REAL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (por_id) REFERENCES purchase_orders (por_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS sales_orders (
			sor_id INTEGER PRIMARY KEY AUTOINCREMENT,
			cus_id INTEGER,
			sor_doc_number TEXT NOT NULL,
			sor_doc_date TEXT,
			sor_ship_date TEXT,
			sor_paid_date TEXT,
			usr_id INTEGER,
			sor_status TEXT,
			sor_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (cus_id) REFERENCES customers (cus_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS sales_order_components (
			soc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sor_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			sor_qty REAL,
			sor_price REAL,
			sor_currency TEXT,
			sor_ship_date TEXT,
			sor_shipped_date TEXT,
			sor_shipped_qty REAL,
			sor_shipped_trackno TEXT,
			soc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sor_id) REFERENCES sales_orders (sor_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run schema statement: %w", err)
		}
	}

	if err := s.ensureColumn(ctx, "boms", "bom_doc_date", "TEXT"); err != nil {
		return err
	}
	for _, migration := range []struct {
		table  string
		column string
		def    string
	}{
		{table: "users", column: "usr_note", def: "TEXT"},
		{table: "customers", column: "cus_note", def: "TEXT"},
		{table: "locations", column: "loc_note", def: "TEXT"},
		{table: "items", column: "itm_note", def: "TEXT"},
		{table: "quotes", column: "qot_note", def: "TEXT"},
		{table: "sales_orders", column: "sor_note", def: "TEXT"},
		{table: "sales_order_components", column: "soc_note", def: "TEXT"},
	} {
		if err := s.ensureColumn(ctx, migration.table, migration.column, migration.def); err != nil {
			return err
		}
	}

	return s.seedDefaults(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, tableName, columnName, columnDef string) error {
	exists, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	statement := fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN %s %s`,
		quoteIdent(tableName),
		quoteIdent(columnName),
		columnDef,
	)
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("add column %s.%s: %w", tableName, columnName, err)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, tableName, columnName string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(tableName)))
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", tableName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return false, fmt.Errorf("scan table info for %s: %w", tableName, err)
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info for %s: %w", tableName, err)
	}
	return false, nil
}

func (s *Store) seedDefaults(ctx context.Context) error {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`)
	var count int
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	defaultUsers := []struct {
		Login string
		Pass  string
		Role  string
	}{
		{Login: "admin", Pass: "admin", Role: "admin"},
		{Login: "user", Pass: "user", Role: "user"},
		{Login: "guest", Pass: "guest", Role: "guest"},
	}

	for _, user := range defaultUsers {
		hash, err := auth.HashPassword(user.Pass)
		if err != nil {
			return fmt.Errorf("hash password for %s: %w", user.Login, err)
		}
		if _, err := s.db.ExecContext(
			ctx,
			`INSERT INTO users (usr_login_name, usr_password, usr_role) VALUES (?, ?, ?)`,
			user.Login,
			hash,
			user.Role,
		); err != nil {
			return fmt.Errorf("seed default user %s: %w", user.Login, err)
		}
	}

	return nil
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read row columns: %w", err)
	}

	records := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for index := range values {
			dest[index] = &values[index]
		}

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		record := make(map[string]any, len(columns))
		for index, column := range columns {
			record[column] = normalizeValue(values[index])
		}
		records = append(records, record)
	}

	return records, rows.Err()
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		clone := make([]byte, len(typed))
		copy(clone, typed)
		return clone
	default:
		return typed
	}
}

func ParseFieldValue(field Field, rawValue string) (any, error) {
	trimmed := strings.TrimSpace(rawValue)
	if trimmed == "" {
		switch field.Kind {
		case KindText, KindTextarea, KindPassword:
			return "", nil
		default:
			return nil, nil
		}
	}

	switch field.Kind {
	case KindInteger, KindForeign:
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case KindReal:
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case KindDate:
		parsed, err := time.Parse(time.DateOnly, trimmed)
		if err != nil {
			return nil, err
		}
		return parsed.Format(time.DateOnly), nil
	default:
		return trimmed, nil
	}
}

func quoteIdent(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func joinQuoted(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quoteIdent(value))
	}
	return strings.Join(quoted, ", ")
}

func selectColumns(table TableDef) []string {
	columns := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		if field.Kind == KindPassword {
			continue
		}
		columns = append(columns, field.Column)
	}
	return columns
}
