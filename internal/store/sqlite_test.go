package store

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOpenSeedsUsersAndHidesPasswordHashes(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	users, err := s.List(context.Background(), "users", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users.Rows) != 3 {
		t.Fatalf("expected 3 default users, got %d", len(users.Rows))
	}
	for _, row := range users.Rows {
		if _, ok := row["usr_password"]; ok {
			t.Fatalf("password hash should not be listed: %+v", row)
		}
	}
}

func TestImportCSVAndBOMDeleteCascadesComponents(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	imported, err := s.ImportCSV(ctx, "customers", strings.NewReader(""+
		"cus_name_en,cus_phone,cus_status\n"+
		"Import One,1000,Active\n"+
		"Import Two,2000,Hold\n"), ParseFieldValue)
	if err != nil {
		t.Fatalf("import customers csv: %v", err)
	}
	if imported != 2 {
		t.Fatalf("imported rows = %d, want 2", imported)
	}

	customers, err := s.List(ctx, "customers", ListOptions{Limit: 10, Sort: "cus_name_en"})
	if err != nil {
		t.Fatalf("list customers: %v", err)
	}
	if len(customers.Rows) != 2 {
		t.Fatalf("expected 2 imported customers, got %d", len(customers.Rows))
	}

	finalItemID, err := s.Insert(ctx, "items", map[string]any{
		"itm_sku":          "FG-001",
		"itm_model":        "Final",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	if err != nil {
		t.Fatalf("insert final item: %v", err)
	}
	partItemID, err := s.Insert(ctx, "items", map[string]any{
		"itm_sku":          "PT-001",
		"itm_model":        "Part",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	if err != nil {
		t.Fatalf("insert part item: %v", err)
	}
	bomID, err := s.Insert(ctx, "boms", map[string]any{
		"bom_doc_number": "BOM-001",
		"itm_id":         finalItemID,
		"bom_status":     "Active",
	})
	if err != nil {
		t.Fatalf("insert bom: %v", err)
	}
	if _, err := s.Insert(ctx, "bom_components", map[string]any{
		"bom_id":  bomID,
		"itm_id":  partItemID,
		"boc_qty": 2.0,
	}); err != nil {
		t.Fatalf("insert bom component: %v", err)
	}

	if err := s.Delete(ctx, "boms", strconv.FormatInt(bomID, 10)); err != nil {
		t.Fatalf("delete bom: %v", err)
	}

	components, err := s.List(ctx, "bom_components", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list bom components: %v", err)
	}
	if len(components.Rows) != 0 {
		t.Fatalf("expected bom components to cascade delete, got %+v", components.Rows)
	}
}

func TestOpenDoesNotMutateProcessTempEnvironment(t *testing.T) {
	sentinelDir := t.TempDir()
	t.Setenv("TMPDIR", sentinelDir)
	t.Setenv("SQLITE_TMPDIR", sentinelDir)

	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	if got := os.Getenv("TMPDIR"); got != sentinelDir {
		t.Fatalf("TMPDIR = %q, want %q", got, sentinelDir)
	}
	if got := os.Getenv("SQLITE_TMPDIR"); got != sentinelDir {
		t.Fatalf("SQLITE_TMPDIR = %q, want %q", got, sentinelDir)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "stockit.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	return store
}
