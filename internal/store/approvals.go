package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrWorkflow marks a workflow rule violation (wrong state, wrong amount) so the
// application layer can answer 400/403 instead of 500.
var ErrWorkflow = errors.New("workflow rule violated")

// ErrWorkflowForbidden marks a workflow step the caller may not take.
var ErrWorkflowForbidden = errors.New("workflow step not allowed for this user")

// decidedStatuses are the two purchase-order statuses only the approve endpoint
// may set. Everything else stays a free transition; these two are the signing
// control, so the generic table API refuses them.
var decidedStatuses = []string{"approved", "rejected"}

type ApprovalSubmission struct {
	PurchaseOrderID int64  `json:"purchase_order_id"`
	Status          string `json:"status"`
	TotalMinor      int64  `json:"total_minor"`
	Currency        string `json:"currency"`
}

type ApprovalDecision struct {
	PurchaseOrderID int64  `json:"purchase_order_id"`
	Status          string `json:"status"`
	TotalMinor      int64  `json:"total_minor"`
	Currency        string `json:"currency"`
	// ApprovalLimitMinor is the deciding user's own ceiling, echoed back so a
	// caller can explain a refusal without a second lookup.
	ApprovalLimitMinor int64 `json:"approval_limit_minor"`
}

// ApprovalLimit reads a user's signing authority: the largest order they may
// approve, in integer minor units. Zero means no approval authority.
func (s *Store) ApprovalLimit(ctx context.Context, userID int64) (int64, error) {
	var limit sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT usr_approval_limit_minor FROM users WHERE usr_id = ?`, userID).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read approval limit: %w", err)
	}
	if !limit.Valid || limit.Int64 < 0 {
		return 0, nil
	}
	return limit.Int64, nil
}

// priceOrder totals a purchase order from its lines and picks a currency for it.
// Lines are summed regardless of currency, so keep one currency per order.
func priceOrder(ctx context.Context, tx *sql.Tx, purchaseOrderID int64, orderCurrency sql.NullString) (int64, sql.NullString, error) {
	var total sql.NullFloat64
	var lineCurrency sql.NullString
	var lineCount int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(COALESCE(poc_qty, 0) * COALESCE(poc_price, 0)), MIN(poc_currency)
		FROM po_components WHERE por_id = ?`, purchaseOrderID).Scan(&lineCount, &total, &lineCurrency)
	if err != nil {
		return 0, orderCurrency, fmt.Errorf("total purchase order lines: %w", err)
	}
	if lineCount == 0 {
		return 0, orderCurrency, fmt.Errorf("%w: a purchase order needs at least one line", ErrWorkflow)
	}
	if strings.TrimSpace(orderCurrency.String) == "" {
		orderCurrency = lineCurrency
	}
	return int64(math.Round(total.Float64 * 100)), orderCurrency, nil
}

// SubmitPurchaseOrder prices a draft order and parks it at pending_approval for
// someone whose signing limit covers it. A user whose own limit already covers
// the order does not need this: they approve it in one step.
func (s *Store) SubmitPurchaseOrder(ctx context.Context, purchaseOrderID int64) (ApprovalSubmission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalSubmission{}, fmt.Errorf("begin submit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	status, currency, payment, err := readOrderForDecision(ctx, tx, purchaseOrderID)
	if err != nil {
		return ApprovalSubmission{}, err
	}
	if current := strings.TrimSpace(status); current != "" && current != "draft" {
		return ApprovalSubmission{}, fmt.Errorf("%w: only a draft purchase order can be submitted (status is %q)", ErrWorkflow, current)
	}

	totalMinor, currency, err := priceOrder(ctx, tx, purchaseOrderID, currency)
	if err != nil {
		return ApprovalSubmission{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE purchase_orders SET por_status = 'pending_approval', por_total_minor = ?, por_currency = ?
		WHERE por_id = ?`, totalMinor, nullableString(currency), purchaseOrderID); err != nil {
		return ApprovalSubmission{}, fmt.Errorf("update purchase order: %w", err)
	}
	if err := insertPOHistoryTx(ctx, tx, purchaseOrderID, status, "pending_approval", payment,
		"submitted for approval", actorFromContext(ctx)); err != nil {
		return ApprovalSubmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApprovalSubmission{}, fmt.Errorf("commit submit: %w", err)
	}

	return ApprovalSubmission{
		PurchaseOrderID: purchaseOrderID,
		Status:          "pending_approval",
		TotalMinor:      totalMinor,
		Currency:        currency.String,
	}, nil
}

// DecidePurchaseOrder approves or rejects a purchase order on the strength of
// the deciding user's own signing limit. The order is repriced from its lines
// first, so the limit is always checked against what the order actually costs.
func (s *Store) DecidePurchaseOrder(ctx context.Context, purchaseOrderID, userID int64, approved bool, note string) (ApprovalDecision, error) {
	limit, err := s.ApprovalLimit(ctx, userID)
	if err != nil {
		return ApprovalDecision{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalDecision{}, fmt.Errorf("begin decide: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	status, currency, payment, err := readOrderForDecision(ctx, tx, purchaseOrderID)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if status != "" && status != "draft" && status != "pending_approval" {
		return ApprovalDecision{}, fmt.Errorf("%w: only a draft or pending purchase order can be decided (status is %q)", ErrWorkflow, status)
	}

	totalMinor, currency, err := priceOrder(ctx, tx, purchaseOrderID, currency)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if limit <= 0 {
		return ApprovalDecision{}, fmt.Errorf("%w: you have no approval authority", ErrWorkflowForbidden)
	}
	if totalMinor > limit {
		return ApprovalDecision{}, fmt.Errorf("%w: this order is %d minor units and your approval limit is %d",
			ErrWorkflowForbidden, totalMinor, limit)
	}

	decided := "approved"
	if !approved {
		decided = "rejected"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE purchase_orders SET por_status = ?, por_total_minor = ?, por_currency = ?
		WHERE por_id = ?`, decided, totalMinor, nullableString(currency), purchaseOrderID); err != nil {
		return ApprovalDecision{}, fmt.Errorf("update purchase order: %w", err)
	}
	historyNote := decided + " within a limit of " + fmt.Sprint(limit)
	if strings.TrimSpace(note) != "" {
		historyNote = note
	}
	if err := insertPOHistoryTx(ctx, tx, purchaseOrderID, status, decided, payment, historyNote, userID); err != nil {
		return ApprovalDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApprovalDecision{}, fmt.Errorf("commit decide: %w", err)
	}

	return ApprovalDecision{
		PurchaseOrderID:    purchaseOrderID,
		Status:             decided,
		TotalMinor:         totalMinor,
		Currency:           currency.String,
		ApprovalLimitMinor: limit,
	}, nil
}

func readOrderForDecision(ctx context.Context, tx *sql.Tx, purchaseOrderID int64) (status string, currency sql.NullString, payment string, err error) {
	var statusValue, paymentValue sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT por_status, por_currency, por_payment_status FROM purchase_orders WHERE por_id = ?`,
		purchaseOrderID).Scan(&statusValue, &currency, &paymentValue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", currency, "", fmt.Errorf("%w: purchase order not found", ErrWorkflow)
	}
	if err != nil {
		return "", currency, "", fmt.Errorf("read purchase order: %w", err)
	}
	return strings.TrimSpace(statusValue.String), currency, paymentValue.String, nil
}

func nullableString(value sql.NullString) any {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	return value.String
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
