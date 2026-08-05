package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

type draft struct {
	ID          int64
	AccountID   int64
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Body        string
	Status      string
	Detail      string
	Attachments []draftAttachment
}

type draftSummary struct {
	ID              int64
	To              []string
	Subject         string
	Status          string
	Detail          string
	UpdatedAt       time.Time
	AttachmentCount int
}

func loadDraftSummaries(ctx context.Context, sqldb *sql.DB, login string) ([]draftSummary, error) {
	rows, err := sqldb.QueryContext(ctx, `SELECT d.id, d.to_json, d.subject, d.status, d.status_detail,
		d.updated_at, COUNT(da.id) FROM drafts d LEFT JOIN draft_attachments da ON da.draft_id=d.id
		WHERE d.login=? AND d.status IN ('draft','error','sending')
		GROUP BY d.id ORDER BY d.updated_at DESC, d.id DESC LIMIT 100`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var drafts []draftSummary
	for rows.Next() {
		var item draftSummary
		var toJSON, updated string
		if err := rows.Scan(&item.ID, &toJSON, &item.Subject, &item.Status, &item.Detail, &updated, &item.AttachmentCount); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(toJSON), &item.To); err != nil {
			return nil, err
		}
		item.UpdatedAt, _ = parseDBTime(updated)
		drafts = append(drafts, item)
	}
	return drafts, rows.Err()
}

func createResponseDraft(ctx context.Context, sqldb *sql.DB, login string, messageID int64, mode string) (int64, error) {
	if mode != "reply" && mode != "reply-all" && mode != "forward" {
		return 0, errors.New("unsupported response mode")
	}
	m, err := loadMessage(ctx, sqldb, login, messageID)
	if err != nil {
		return 0, err
	}
	var accountID int64
	var ownAddress string
	if err := sqldb.QueryRowContext(ctx, `SELECT a.id, a.email FROM messages m JOIN accounts a ON a.id=m.account_id
		WHERE m.id=? AND a.login=?`, messageID, login).Scan(&accountID, &ownAddress); err != nil {
		return 0, err
	}
	var to, cc []string
	subject := m.Subject
	body := ""
	switch mode {
	case "reply", "reply-all":
		replyAddress := m.FromAddress
		if len(m.ReplyTo) > 0 {
			replyAddress = m.ReplyTo[0]
		}
		to = appendUniqueAddress(to, replyAddress, ownAddress)
		if mode == "reply-all" {
			for _, value := range append(append([]string{}, m.To...), m.Cc...) {
				if !containsAddress(to, value) {
					cc = appendUniqueAddress(cc, value, ownAddress)
				}
			}
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), "re:") {
			subject = "Re: " + subject
		}
		body = fmt.Sprintf("\n\nOn %s, %s wrote:\n%s", m.ReceivedAt.Local().Format("Jan 2, 2006 at 3:04 PM"),
			m.FromAddress, quoteText(m.TextBody))
	case "forward":
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), "fwd:") {
			subject = "Fwd: " + subject
		}
		body = fmt.Sprintf("\n\n---------- Forwarded message ----------\nFrom: %s\nDate: %s\nSubject: %s\nTo: %s\n\n%s",
			m.FromAddress, m.ReceivedAt.Local().Format(time.RFC1123), m.Subject, strings.Join(m.To, ", "), m.TextBody)
	}
	toJSON, _ := json.Marshal(to)
	ccJSON, _ := json.Marshal(cc)
	result, err := sqldb.ExecContext(ctx, `INSERT INTO drafts
		(login, account_id, reply_to_message_id, mode, to_json, cc_json, subject, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, login, accountID, messageID, mode, toJSON, ccJSON, subject, body)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func containsAddress(values []string, candidate string) bool {
	parsed, err := mail.ParseAddress(candidate)
	if err != nil {
		return false
	}
	for _, value := range values {
		existing, err := mail.ParseAddress(value)
		if err == nil && strings.EqualFold(existing.Address, parsed.Address) {
			return true
		}
	}
	return false
}

func appendUniqueAddress(values []string, candidate, own string) []string {
	parsed, err := mail.ParseAddress(candidate)
	if err != nil || strings.EqualFold(parsed.Address, own) {
		return values
	}
	for _, existing := range values {
		address, err := mail.ParseAddress(existing)
		if err == nil && strings.EqualFold(address.Address, parsed.Address) {
			return values
		}
	}
	return append(values, parsed.String())
}

func quoteText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = "> " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func loadDraft(ctx context.Context, sqldb *sql.DB, login string, id int64) (draft, error) {
	var d draft
	var accountID sql.NullInt64
	var toJSON, ccJSON, bccJSON string
	err := sqldb.QueryRowContext(ctx, `SELECT id, account_id, to_json, cc_json, bcc_json,
		subject, body, status, status_detail FROM drafts WHERE id=? AND login=?`, id, login).
		Scan(&d.ID, &accountID, &toJSON, &ccJSON, &bccJSON, &d.Subject, &d.Body, &d.Status, &d.Detail)
	if err != nil {
		return draft{}, err
	}
	if accountID.Valid {
		d.AccountID = accountID.Int64
	}
	if err := json.Unmarshal([]byte(toJSON), &d.To); err != nil {
		return draft{}, err
	}
	if err := json.Unmarshal([]byte(ccJSON), &d.Cc); err != nil {
		return draft{}, err
	}
	if err := json.Unmarshal([]byte(bccJSON), &d.Bcc); err != nil {
		return draft{}, err
	}
	d.Attachments, err = loadDraftAttachmentData(ctx, sqldb, login, id)
	if err != nil {
		return draft{}, err
	}
	return d, nil
}

func saveDraft(ctx context.Context, sqldb *sql.DB, login string, id int64, accountID int64, to, cc, bcc, subject, body string) (int64, error) {
	toList, err := parseAddressInput(to, false)
	if err != nil {
		return 0, fmt.Errorf("to: %w", err)
	}
	ccList, err := parseAddressInput(cc, false)
	if err != nil {
		return 0, fmt.Errorf("cc: %w", err)
	}
	bccList, err := parseAddressInput(bcc, false)
	if err != nil {
		return 0, fmt.Errorf("bcc: %w", err)
	}
	toJSON, _ := json.Marshal(toList)
	ccJSON, _ := json.Marshal(ccList)
	bccJSON, _ := json.Marshal(bccList)
	if accountID != 0 {
		var exists int
		if err := sqldb.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE id=? AND login=? AND status!='disabled'", accountID, login).Scan(&exists); err != nil {
			return 0, errors.New("choose a connected sending account")
		}
	}
	if id == 0 {
		result, err := sqldb.ExecContext(ctx, `INSERT INTO drafts
			(login, account_id, to_json, cc_json, bcc_json, subject, body)
			VALUES (?, NULLIF(?,0), ?, ?, ?, ?, ?)`, login, accountID, toJSON, ccJSON, bccJSON,
			strings.TrimSpace(subject), body)
		if err != nil {
			return 0, err
		}
		return result.LastInsertId()
	}
	result, err := sqldb.ExecContext(ctx, `UPDATE drafts SET account_id=NULLIF(?,0),
		to_json=?, cc_json=?, bcc_json=?, subject=?, body=?, status='draft', status_detail='',
		updated_at=datetime('now') WHERE id=? AND login=? AND status IN ('draft','error')`, accountID, toJSON, ccJSON, bccJSON,
		strings.TrimSpace(subject), body, id, login)
	if err != nil {
		return 0, err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return 0, sql.ErrNoRows
	}
	return id, nil
}

func parseAddressInput(value string, requireOne bool) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if requireOne {
			return nil, errors.New("at least one recipient is required")
		}
		return []string{}, nil
	}
	addresses, err := mail.ParseAddressList(value)
	if err != nil {
		return nil, errors.New("enter comma-separated email addresses")
	}
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, address.String())
	}
	return out, nil
}
