package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrWorkflow marks a workflow rule violation (wrong state, wrong role) so the
// application layer can answer 400/403 instead of 500.
var ErrWorkflow = errors.New("workflow rule violated")

// ErrWorkflowForbidden marks a workflow step the caller's role may not take.
var ErrWorkflowForbidden = errors.New("workflow step not allowed for this role")

// purchaseOrderSource is the only document type that routes through approvals.
// It stays a stored value on approvals and approval_rules so a second document
// type can be added later without a schema change.
const purchaseOrderSource = "purchase_order"

type ApprovalSubmission struct {
	SourceType string           `json:"source_type"`
	SourceID   int64            `json:"source_id"`
	Status     string           `json:"status"`
	TotalMinor int64            `json:"total_minor"`
	Currency   string           `json:"currency"`
	Approvals  []map[string]any `json:"approvals"`
}

type ApprovalDecision struct {
	Approval           map[string]any `json:"approval"`
	SourceType         string         `json:"source_type"`
	SourceID           int64          `json:"source_id"`
	Status             string         `json:"status"`
	RemainingApprovals int            `json:"remaining_approvals"`
}

// SubmitPurchaseOrder prices a draft purchase order from its lines, matches the
// active approval rules for that amount, and records one pending approval step
// per matching rule. An order that matches no rule needs no approval and is
// approved outright.
func (s *Store) SubmitPurchaseOrder(ctx context.Context, purchaseOrderID int64) (ApprovalSubmission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalSubmission{}, fmt.Errorf("begin submit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status, currency, payment sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT por_status, por_currency, por_payment_status FROM purchase_orders WHERE por_id = ?`,
		purchaseOrderID).Scan(&status, &currency, &payment)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalSubmission{}, fmt.Errorf("%w: purchase order not found", ErrWorkflow)
	}
	if err != nil {
		return ApprovalSubmission{}, fmt.Errorf("read purchase order: %w", err)
	}
	if current := strings.TrimSpace(status.String); current != "" && current != "draft" {
		return ApprovalSubmission{}, fmt.Errorf("%w: only a draft purchase order can be submitted (status is %q)", ErrWorkflow, current)
	}

	var total sql.NullFloat64
	var lineCurrency sql.NullString
	var lineCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(COALESCE(poc_qty, 0) * COALESCE(poc_price, 0)), MIN(poc_currency)
		FROM po_components WHERE por_id = ?`, purchaseOrderID).Scan(&lineCount, &total, &lineCurrency)
	if err != nil {
		return ApprovalSubmission{}, fmt.Errorf("total purchase order lines: %w", err)
	}
	if lineCount == 0 {
		return ApprovalSubmission{}, fmt.Errorf("%w: a purchase order needs at least one line before submission", ErrWorkflow)
	}
	totalMinor := int64(math.Round(total.Float64 * 100))
	if strings.TrimSpace(currency.String) == "" {
		currency = lineCurrency
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM approvals WHERE apv_source_type = ? AND apv_source_id = ?`,
		purchaseOrderSource, purchaseOrderID); err != nil {
		return ApprovalSubmission{}, fmt.Errorf("clear previous approvals: %w", err)
	}

	rules, err := tx.QueryContext(ctx, `
		SELECT apr_step, apr_role FROM approval_rules
		WHERE apr_source_type = ? AND apr_status = 'active' AND apr_min_amount_minor <= ?
		ORDER BY apr_step, apr_id`, purchaseOrderSource, totalMinor)
	if err != nil {
		return ApprovalSubmission{}, fmt.Errorf("match approval rules: %w", err)
	}
	type step struct {
		number int64
		role   string
	}
	steps := []step{}
	for rules.Next() {
		var next step
		if err := rules.Scan(&next.number, &next.role); err != nil {
			_ = rules.Close()
			return ApprovalSubmission{}, fmt.Errorf("scan approval rule: %w", err)
		}
		steps = append(steps, next)
	}
	if err := rules.Err(); err != nil {
		_ = rules.Close()
		return ApprovalSubmission{}, fmt.Errorf("read approval rules: %w", err)
	}
	_ = rules.Close()

	for _, next := range steps {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approvals (apv_source_type, apv_source_id, apv_step, apv_role, apv_status)
			VALUES (?, ?, ?, ?, 'pending')`, purchaseOrderSource, purchaseOrderID, next.number, next.role); err != nil {
			return ApprovalSubmission{}, fmt.Errorf("create approval step: %w", err)
		}
	}

	newStatus := "approved"
	if len(steps) > 0 {
		newStatus = "pending_approval"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE purchase_orders SET por_status = ?, por_total_minor = ?, por_currency = ? WHERE por_id = ?`,
		newStatus, totalMinor, nullableString(currency), purchaseOrderID); err != nil {
		return ApprovalSubmission{}, fmt.Errorf("update purchase order: %w", err)
	}
	if err := insertPOHistoryTx(ctx, tx, purchaseOrderID, status.String, newStatus, payment.String,
		fmt.Sprintf("submitted for approval (%d step(s))", len(steps)), actorFromContext(ctx)); err != nil {
		return ApprovalSubmission{}, err
	}

	approvals, err := selectApprovals(ctx, tx, purchaseOrderID)
	if err != nil {
		return ApprovalSubmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApprovalSubmission{}, fmt.Errorf("commit submit: %w", err)
	}

	return ApprovalSubmission{
		SourceType: purchaseOrderSource,
		SourceID:   purchaseOrderID,
		Status:     newStatus,
		TotalMinor: totalMinor,
		Currency:   currency.String,
		Approvals:  approvals,
	}, nil
}

// DecideApproval records one approval step. Steps are decided in order, only by
// a user holding the step's role, and a rejection stops the whole order.
func (s *Store) DecideApproval(ctx context.Context, approvalID int64, role string, userID int64, approved bool, note string) (ApprovalDecision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalDecision{}, fmt.Errorf("begin decide: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sourceType, stepRole, stepStatus string
	var sourceID, stepNumber int64
	err = tx.QueryRowContext(ctx, `
		SELECT apv_source_type, apv_source_id, apv_step, apv_role, apv_status
		FROM approvals WHERE apv_id = ?`, approvalID).Scan(&sourceType, &sourceID, &stepNumber, &stepRole, &stepStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalDecision{}, fmt.Errorf("%w: approval not found", ErrWorkflow)
	}
	if err != nil {
		return ApprovalDecision{}, fmt.Errorf("read approval: %w", err)
	}
	if sourceType != purchaseOrderSource {
		return ApprovalDecision{}, fmt.Errorf("%w: unknown approval source %q", ErrWorkflow, sourceType)
	}
	if stepStatus != "pending" {
		return ApprovalDecision{}, fmt.Errorf("%w: approval %d was already %s", ErrWorkflow, approvalID, stepStatus)
	}
	if stepRole != role {
		return ApprovalDecision{}, fmt.Errorf("%w: this step must be decided by role %q", ErrWorkflowForbidden, stepRole)
	}

	var earlier int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM approvals
		WHERE apv_source_type = ? AND apv_source_id = ? AND apv_status = 'pending' AND apv_step < ?`,
		sourceType, sourceID, stepNumber).Scan(&earlier); err != nil {
		return ApprovalDecision{}, fmt.Errorf("check earlier approvals: %w", err)
	}
	if earlier > 0 {
		return ApprovalDecision{}, fmt.Errorf("%w: an earlier approval step is still pending", ErrWorkflow)
	}

	decision := "rejected"
	if approved {
		decision = "approved"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE approvals SET apv_status = ?, apv_decided_by = ?, apv_decided_at = CURRENT_TIMESTAMP, apv_note = ?
		WHERE apv_id = ?`, decision, userID, nullableText(note), approvalID); err != nil {
		return ApprovalDecision{}, fmt.Errorf("record decision: %w", err)
	}

	var remaining int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM approvals
		WHERE apv_source_type = ? AND apv_source_id = ? AND apv_status = 'pending'`,
		sourceType, sourceID).Scan(&remaining); err != nil {
		return ApprovalDecision{}, fmt.Errorf("count remaining approvals: %w", err)
	}

	orderStatus := "pending_approval"
	switch {
	case !approved:
		orderStatus = "rejected"
	case remaining == 0:
		orderStatus = "approved"
	}
	if orderStatus != "pending_approval" {
		var payment sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT por_payment_status FROM purchase_orders WHERE por_id = ?`, sourceID).Scan(&payment); err != nil {
			return ApprovalDecision{}, fmt.Errorf("read purchase order payment status: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE purchase_orders SET por_status = ? WHERE por_id = ?`, orderStatus, sourceID); err != nil {
			return ApprovalDecision{}, fmt.Errorf("update purchase order status: %w", err)
		}
		historyNote := fmt.Sprintf("approval step %d %s", stepNumber, decision)
		if strings.TrimSpace(note) != "" {
			historyNote += ": " + note
		}
		if err := insertPOHistoryTx(ctx, tx, sourceID, "pending_approval", orderStatus, payment.String, historyNote, userID); err != nil {
			return ApprovalDecision{}, err
		}
	}

	row, err := scanSingle(ctx, tx, `
		SELECT apv_id, apv_source_type, apv_source_id, apv_step, apv_role, apv_status, apv_decided_by, apv_decided_at, apv_note, created_at
		FROM approvals WHERE apv_id = ?`, approvalID)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApprovalDecision{}, fmt.Errorf("commit decide: %w", err)
	}

	return ApprovalDecision{
		Approval:           row,
		SourceType:         purchaseOrderSource,
		SourceID:           sourceID,
		Status:             orderStatus,
		RemainingApprovals: remaining,
	}, nil
}

func selectApprovals(ctx context.Context, tx *sql.Tx, purchaseOrderID int64) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT apv_id, apv_source_type, apv_source_id, apv_step, apv_role, apv_status, apv_decided_by, apv_decided_at, apv_note, created_at
		FROM approvals WHERE apv_source_type = ? AND apv_source_id = ? ORDER BY apv_step, apv_id`,
		purchaseOrderSource, purchaseOrderID)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanSingle(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[string]any, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read row: %w", err)
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
