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

const requisitionSource = "purchase_requisition"

type RequisitionSubmission struct {
	RequisitionID int64            `json:"requisition_id"`
	Status        string           `json:"status"`
	TotalMinor    int64            `json:"total_minor"`
	Currency      string           `json:"currency"`
	Approvals     []map[string]any `json:"approvals"`
}

type ApprovalDecision struct {
	Approval           map[string]any `json:"approval"`
	RequisitionID      int64          `json:"requisition_id"`
	RequisitionStatus  string         `json:"requisition_status"`
	RemainingApprovals int            `json:"remaining_approvals"`
}

// SubmitRequisition prices a draft requisition from its lines, matches the
// active approval rules for that amount, and records one pending approval step
// per matching rule. A requisition that matches no rule needs no approval and
// is approved outright.
func (s *Store) SubmitRequisition(ctx context.Context, requisitionID int64) (RequisitionSubmission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RequisitionSubmission{}, fmt.Errorf("begin submit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status, currency sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT prq_status, prq_currency FROM purchase_requisitions WHERE prq_id = ?`, requisitionID).Scan(&status, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return RequisitionSubmission{}, fmt.Errorf("%w: requisition not found", ErrWorkflow)
	}
	if err != nil {
		return RequisitionSubmission{}, fmt.Errorf("read requisition: %w", err)
	}
	if current := strings.TrimSpace(status.String); current != "" && current != "draft" {
		return RequisitionSubmission{}, fmt.Errorf("%w: only a draft requisition can be submitted (status is %q)", ErrWorkflow, current)
	}

	var total sql.NullFloat64
	var lineCurrency sql.NullString
	var lineCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(COALESCE(prc_qty, 0) * COALESCE(prc_price, 0)), MIN(prc_currency)
		FROM prq_components WHERE prq_id = ?`, requisitionID).Scan(&lineCount, &total, &lineCurrency)
	if err != nil {
		return RequisitionSubmission{}, fmt.Errorf("total requisition lines: %w", err)
	}
	if lineCount == 0 {
		return RequisitionSubmission{}, fmt.Errorf("%w: a requisition needs at least one line before submission", ErrWorkflow)
	}
	totalMinor := int64(math.Round(total.Float64 * 100))
	if strings.TrimSpace(currency.String) == "" {
		currency = lineCurrency
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM approvals WHERE apv_source_type = ? AND apv_source_id = ?`, requisitionSource, requisitionID); err != nil {
		return RequisitionSubmission{}, fmt.Errorf("clear previous approvals: %w", err)
	}

	rules, err := tx.QueryContext(ctx, `
		SELECT apr_step, apr_role FROM approval_rules
		WHERE apr_source_type = ? AND apr_status = 'active' AND apr_min_amount_minor <= ?
		ORDER BY apr_step, apr_id`, requisitionSource, totalMinor)
	if err != nil {
		return RequisitionSubmission{}, fmt.Errorf("match approval rules: %w", err)
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
			return RequisitionSubmission{}, fmt.Errorf("scan approval rule: %w", err)
		}
		steps = append(steps, next)
	}
	if err := rules.Err(); err != nil {
		_ = rules.Close()
		return RequisitionSubmission{}, fmt.Errorf("read approval rules: %w", err)
	}
	_ = rules.Close()

	for _, next := range steps {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approvals (apv_source_type, apv_source_id, apv_step, apv_role, apv_status)
			VALUES (?, ?, ?, ?, 'pending')`, requisitionSource, requisitionID, next.number, next.role); err != nil {
			return RequisitionSubmission{}, fmt.Errorf("create approval step: %w", err)
		}
	}

	newStatus := "approved"
	if len(steps) > 0 {
		newStatus = "submitted"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE purchase_requisitions SET prq_status = ?, prq_total_minor = ?, prq_currency = ?
		WHERE prq_id = ?`, newStatus, totalMinor, nullableString(currency), requisitionID); err != nil {
		return RequisitionSubmission{}, fmt.Errorf("update requisition: %w", err)
	}

	approvals, err := selectApprovals(ctx, tx, requisitionID)
	if err != nil {
		return RequisitionSubmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return RequisitionSubmission{}, fmt.Errorf("commit submit: %w", err)
	}

	return RequisitionSubmission{
		RequisitionID: requisitionID,
		Status:        newStatus,
		TotalMinor:    totalMinor,
		Currency:      currency.String,
		Approvals:     approvals,
	}, nil
}

// DecideApproval records one approval step. Steps are decided in order, only by
// a user holding the step's role, and a rejection stops the whole requisition.
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

	requisitionStatus := "submitted"
	switch {
	case !approved:
		requisitionStatus = "rejected"
	case remaining == 0:
		requisitionStatus = "approved"
	}
	if requisitionStatus != "submitted" {
		if _, err := tx.ExecContext(ctx, `UPDATE purchase_requisitions SET prq_status = ? WHERE prq_id = ?`, requisitionStatus, sourceID); err != nil {
			return ApprovalDecision{}, fmt.Errorf("update requisition status: %w", err)
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
		RequisitionID:      sourceID,
		RequisitionStatus:  requisitionStatus,
		RemainingApprovals: remaining,
	}, nil
}

// CreatePOFromRequisition turns an approved requisition into a draft purchase
// order with the same lines, and links the two records.
func (s *Store) CreatePOFromRequisition(ctx context.Context, requisitionID int64, docNumber string, supplierID *int64, userID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin create purchase order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status, requisitionDoc sql.NullString
	var requisitionSupplier, existingPO sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT prq_status, prq_doc_number, sup_id, por_id FROM purchase_requisitions WHERE prq_id = ?`,
		requisitionID).Scan(&status, &requisitionDoc, &requisitionSupplier, &existingPO)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: requisition not found", ErrWorkflow)
	}
	if err != nil {
		return 0, fmt.Errorf("read requisition: %w", err)
	}
	if strings.TrimSpace(status.String) != "approved" {
		return 0, fmt.Errorf("%w: only an approved requisition can become a purchase order (status is %q)", ErrWorkflow, status.String)
	}
	if existingPO.Valid {
		return 0, fmt.Errorf("%w: requisition already became purchase order %d", ErrWorkflow, existingPO.Int64)
	}

	if strings.TrimSpace(docNumber) == "" {
		docNumber = requisitionDoc.String
	}
	supplier := requisitionSupplier
	if supplierID != nil {
		supplier = sql.NullInt64{Int64: *supplierID, Valid: true}
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO purchase_orders (sup_id, por_doc_number, por_doc_date, usr_id, por_status)
		VALUES (?, ?, date('now'), ?, 'draft')`, supplier, docNumber, userID)
	if err != nil {
		return 0, fmt.Errorf("create purchase order: %w", err)
	}
	purchaseOrderID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read new purchase order id: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO po_components (por_id, itm_id, poc_qty, poc_price, poc_currency)
		SELECT ?, itm_id, prc_qty, prc_price, prc_currency FROM prq_components WHERE prq_id = ? ORDER BY prc_id`,
		purchaseOrderID, requisitionID); err != nil {
		return 0, fmt.Errorf("copy requisition lines: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE purchase_requisitions SET prq_status = 'ordered', por_id = ? WHERE prq_id = ?`,
		purchaseOrderID, requisitionID); err != nil {
		return 0, fmt.Errorf("link requisition to purchase order: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit create purchase order: %w", err)
	}
	return purchaseOrderID, nil
}

func selectApprovals(ctx context.Context, tx *sql.Tx, requisitionID int64) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT apv_id, apv_source_type, apv_source_id, apv_step, apv_role, apv_status, apv_decided_by, apv_decided_at, apv_note, created_at
		FROM approvals WHERE apv_source_type = ? AND apv_source_id = ? ORDER BY apv_step, apv_id`,
		requisitionSource, requisitionID)
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
