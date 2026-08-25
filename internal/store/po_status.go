package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type actorContextKey struct{}

// WithActor tags a context with the acting user so the store can attribute the
// audit rows it writes on its own (purchase-order status history). Handlers set
// it once next to the request principal; every store call below inherits it.
func WithActor(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, actorContextKey{}, userID)
}

func actorFromContext(ctx context.Context) any {
	userID, ok := ctx.Value(actorContextKey{}).(int64)
	if !ok || userID <= 0 {
		return nil
	}
	return userID
}

// poReceiptStatuses are the lifecycle states a goods receipt may move on its
// own. Anything else (draft, approval states, on_hold, invoiced, closed,
// cancelled) is left to a human decision.
var poReceiptStatuses = []string{"issued", "confirmed", "partially_received", "received"}

type poStatusSnapshot struct {
	status  string
	payment string
}

func (s *Store) purchaseOrderSnapshot(ctx context.Context, id string) (poStatusSnapshot, bool) {
	var status, payment sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT por_status, por_payment_status FROM purchase_orders WHERE por_id = ?`, id).Scan(&status, &payment)
	if err != nil {
		return poStatusSnapshot{}, false
	}
	return poStatusSnapshot{status: status.String, payment: payment.String}, true
}

// recordPOStatusChange appends one history row when the status or the payment
// tag actually moved. Callers pass the snapshot taken before their write.
func (s *Store) recordPOStatusChange(ctx context.Context, id string, before poStatusSnapshot, note string) error {
	after, ok := s.purchaseOrderSnapshot(ctx, id)
	if !ok || (after.status == before.status && after.payment == before.payment) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO po_status_history (por_id, psh_previous_status, psh_status, psh_payment_status, usr_id, psh_note)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, nullableText(before.status), after.status, nullableText(after.payment), actorFromContext(ctx), nullableText(note))
	if err != nil {
		return fmt.Errorf("record purchase order status change: %w", err)
	}
	return nil
}

// insertPOHistoryTx writes one history row inside an open transaction, for the
// workflow steps that change a purchase order's status as part of a larger unit.
func insertPOHistoryTx(ctx context.Context, tx *sql.Tx, porID int64, previous, status, payment, note string, userID any) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO po_status_history (por_id, psh_previous_status, psh_status, psh_payment_status, usr_id, psh_note)
		VALUES (?, ?, ?, ?, ?, ?)`,
		porID, nullableText(previous), status, nullableText(payment), userID, nullableText(note))
	if err != nil {
		return fmt.Errorf("record purchase order status change: %w", err)
	}
	return nil
}

// SetPOStatus changes a purchase order's status and/or payment tag and records
// the change with a note. Transitions are deliberately unrestricted: the history
// is the control, not a state machine. An empty value leaves that field alone.
func (s *Store) SetPOStatus(ctx context.Context, purchaseOrderID int64, status, paymentStatus, note string) (map[string]any, error) {
	id := strconv.FormatInt(purchaseOrderID, 10)
	status = strings.TrimSpace(status)
	paymentStatus = strings.TrimSpace(paymentStatus)
	if status == "" && paymentStatus == "" {
		return nil, fmt.Errorf("%w: give a status, a payment_status, or both", ErrWorkflow)
	}
	if status != "" && !slices.Contains(poStatusOptions, status) {
		return nil, fmt.Errorf("%w: unknown purchase order status %q", ErrWorkflow, status)
	}
	if paymentStatus != "" && !slices.Contains(paymentStatusOptions, paymentStatus) {
		return nil, fmt.Errorf("%w: unknown payment status %q", ErrWorkflow, paymentStatus)
	}

	before, ok := s.purchaseOrderSnapshot(ctx, id)
	if !ok {
		return nil, fmt.Errorf("%w: purchase order not found", ErrWorkflow)
	}

	assignments := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if status != "" {
		assignments = append(assignments, "por_status = ?")
		args = append(args, status)
	}
	if paymentStatus != "" {
		assignments = append(assignments, "por_payment_status = ?")
		args = append(args, paymentStatus)
	}
	args = append(args, purchaseOrderID)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE purchase_orders SET `+strings.Join(assignments, ", ")+` WHERE por_id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update purchase order status: %w", err)
	}
	if err := s.recordPOStatusChange(ctx, id, before, note); err != nil {
		return nil, err
	}
	return s.Get(ctx, "purchase_orders", id)
}

// syncPOReceiptStatus derives partially_received / received from the order's
// line receipts. It only moves an order that is already in the fulfilment part
// of the lifecycle, so a draft or a closed order is never disturbed.
func (s *Store) syncPOReceiptStatus(ctx context.Context, purchaseOrderID string) error {
	if strings.TrimSpace(purchaseOrderID) == "" {
		return nil
	}
	before, ok := s.purchaseOrderSnapshot(ctx, purchaseOrderID)
	if !ok || !slices.Contains(poReceiptStatuses, before.status) {
		return nil
	}

	var ordered, received sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(poc_qty, 0)), SUM(COALESCE(poc_received_qty, 0))
		FROM po_components WHERE por_id = ?`, purchaseOrderID).Scan(&ordered, &received); err != nil {
		return fmt.Errorf("total purchase order receipts: %w", err)
	}
	if received.Float64 <= 0 {
		return nil
	}
	next := "partially_received"
	if ordered.Float64 > 0 && received.Float64 >= ordered.Float64 {
		next = "received"
	}
	if next == before.status {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE purchase_orders SET por_status = ? WHERE por_id = ?`, next, purchaseOrderID); err != nil {
		return fmt.Errorf("update purchase order receipt status: %w", err)
	}
	return s.recordPOStatusChange(ctx, purchaseOrderID, before, "derived from line receipts")
}

// componentParentID reads the purchase order a line belongs to, so a line write
// can resync its order's receipt status.
func (s *Store) componentParentID(ctx context.Context, componentID string) string {
	var porID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT por_id FROM po_components WHERE poc_id = ?`, componentID).Scan(&porID)
	if err != nil || !porID.Valid {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		return ""
	}
	return strconv.FormatInt(porID.Int64, 10)
}
