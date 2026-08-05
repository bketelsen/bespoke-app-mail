package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type account struct {
	ID           int64
	Provider     string
	Email        string
	DisplayName  string
	Status       string
	StatusDetail string
	LastSyncAt   *time.Time
}

type mailbox struct {
	ID          int64
	AccountID   int64
	AccountName string
	Name        string
	Role        string
	Unread      int
}

type messageSummary struct {
	ID             int64
	MailboxID      int64
	FromName       string
	FromAddress    string
	Subject        string
	Preview        string
	ReceivedAt     time.Time
	IsRead         bool
	IsStarred      bool
	HasAttachments bool
}

type workspaceData struct {
	Accounts  []account
	Mailboxes []mailbox
	Messages  []messageSummary
	Selected  *messageDetail
	MailboxID int64
	Live      bool
}

type messageDetail struct {
	messageSummary
	To          []string
	Cc          []string
	ReplyTo     []string
	TextBody    string
	HTMLBody    string
	BodyFetched bool
	Attachments []attachment
}

type attachment struct {
	ID          int64
	Filename    string
	ContentType string
	SizeBytes   int64
}

type disconnectedAccount struct {
	Provider string
	Email    string
}

func disconnectMailAccount(ctx context.Context, sqldb *sql.DB, login string, accountID int64, confirmed bool) (disconnectedAccount, error) {
	if !confirmed {
		return disconnectedAccount{}, errors.New("account disconnect must be explicitly confirmed")
	}
	var removed disconnectedAccount
	err := sqldb.QueryRowContext(ctx, `DELETE FROM accounts WHERE id=? AND login=?
		RETURNING provider, email`, accountID, login).Scan(&removed.Provider, &removed.Email)
	if err != nil {
		return disconnectedAccount{}, err
	}
	return removed, nil
}

func loadAccounts(ctx context.Context, sqldb *sql.DB, login string) ([]account, error) {
	rows, err := sqldb.QueryContext(ctx, `
		SELECT id, provider, email, display_name, status, status_detail, last_sync_at
		FROM accounts WHERE login=? ORDER BY email`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []account
	for rows.Next() {
		var a account
		var lastSync sql.NullString
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.DisplayName, &a.Status, &a.StatusDetail, &lastSync); err != nil {
			return nil, err
		}
		if lastSync.Valid {
			if parsed, ok := parseDBTime(lastSync.String); ok {
				a.LastSyncAt = &parsed
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadWorkspace(ctx context.Context, sqldb *sql.DB, login string, mailboxID, messageID int64) (workspaceData, error) {
	accounts, err := loadAccounts(ctx, sqldb, login)
	if err != nil {
		return workspaceData{}, err
	}
	d := workspaceData{Accounts: accounts, MailboxID: mailboxID, Live: mailboxID == 0 && messageID == 0}

	rows, err := sqldb.QueryContext(ctx, `
		SELECT mb.id, a.id, a.email, mb.display_name, mb.role, mb.unread_count
		FROM mailboxes mb JOIN accounts a ON a.id=mb.account_id
		WHERE a.login=? ORDER BY a.email, mb.position, mb.display_name`, login)
	if err != nil {
		return workspaceData{}, err
	}
	for rows.Next() {
		var mb mailbox
		if err := rows.Scan(&mb.ID, &mb.AccountID, &mb.AccountName, &mb.Name, &mb.Role, &mb.Unread); err != nil {
			rows.Close()
			return workspaceData{}, err
		}
		d.Mailboxes = append(d.Mailboxes, mb)
	}
	if err := rows.Close(); err != nil {
		return workspaceData{}, err
	}
	mrows, err := sqldb.QueryContext(ctx, `
		SELECT m.id, mm.mailbox_id,
			COALESCE((SELECT name FROM addresses WHERE message_id=m.id AND kind='from' ORDER BY position LIMIT 1), ''),
			COALESCE((SELECT address FROM addresses WHERE message_id=m.id AND kind='from' ORDER BY position LIMIT 1), ''),
			m.subject, m.preview, m.received_at, m.is_read, m.is_starred, m.has_attachments
		FROM messages m JOIN message_mailboxes mm ON mm.message_id=m.id
		JOIN mailboxes mb ON mb.id=mm.mailbox_id JOIN accounts a ON a.id=m.account_id
		WHERE ((?=0 AND mb.role='inbox') OR mm.mailbox_id=?) AND a.login=?
		AND NOT EXISTS (SELECT 1 FROM pending_operations po WHERE po.message_id=m.id
			AND po.kind IN ('archive','trash'))
		ORDER BY m.received_at DESC LIMIT 100`, d.MailboxID, d.MailboxID, login)
	if err != nil {
		return workspaceData{}, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m messageSummary
		var received string
		if err := mrows.Scan(&m.ID, &m.MailboxID, &m.FromName, &m.FromAddress, &m.Subject, &m.Preview,
			&received, &m.IsRead, &m.IsStarred, &m.HasAttachments); err != nil {
			return workspaceData{}, err
		}
		m.ReceivedAt, _ = parseDBTime(received)
		d.Messages = append(d.Messages, m)
	}
	if err := mrows.Err(); err != nil {
		return workspaceData{}, err
	}
	if messageID != 0 {
		selected, err := loadMessage(ctx, sqldb, login, messageID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return workspaceData{}, err
		}
		if selected != nil {
			selected.MailboxID = d.MailboxID
		}
		d.Selected = selected
	}
	return d, nil
}

func loadMessage(ctx context.Context, sqldb *sql.DB, login string, id int64) (*messageDetail, error) {
	var m messageDetail
	var received string
	err := sqldb.QueryRowContext(ctx, `
		SELECT m.id, COALESCE(mm.mailbox_id, 0),
			COALESCE((SELECT name FROM addresses WHERE message_id=m.id AND kind='from' ORDER BY position LIMIT 1), ''),
			COALESCE((SELECT address FROM addresses WHERE message_id=m.id AND kind='from' ORDER BY position LIMIT 1), ''),
			m.subject, m.preview, m.received_at, m.is_read, m.is_starred, m.has_attachments,
			m.text_body, m.html_body, m.body_fetched
		FROM messages m JOIN accounts a ON a.id=m.account_id
		LEFT JOIN message_mailboxes mm ON mm.message_id=m.id
		WHERE m.id=? AND a.login=? LIMIT 1`, id, login).Scan(
		&m.ID, &m.MailboxID, &m.FromName, &m.FromAddress, &m.Subject, &m.Preview, &received,
		&m.IsRead, &m.IsStarred, &m.HasAttachments, &m.TextBody, &m.HTMLBody, &m.BodyFetched)
	if err != nil {
		return nil, err
	}
	m.ReceivedAt, _ = parseDBTime(received)
	rows, err := sqldb.QueryContext(ctx, `SELECT kind, name, address FROM addresses
		WHERE message_id=? AND kind IN ('to','cc','reply-to') ORDER BY kind, position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, address string
		if err := rows.Scan(&kind, &name, &address); err != nil {
			return nil, err
		}
		value := address
		if name != "" {
			value = fmt.Sprintf("%s <%s>", name, address)
		}
		switch kind {
		case "to":
			m.To = append(m.To, value)
		case "cc":
			m.Cc = append(m.Cc, value)
		case "reply-to":
			m.ReplyTo = append(m.ReplyTo, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	attachmentRows, err := sqldb.QueryContext(ctx, `SELECT id, filename, content_type, size_bytes
		FROM attachments WHERE message_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer attachmentRows.Close()
	for attachmentRows.Next() {
		var item attachment
		if err := attachmentRows.Scan(&item.ID, &item.Filename, &item.ContentType, &item.SizeBytes); err != nil {
			return nil, err
		}
		m.Attachments = append(m.Attachments, item)
	}
	return &m, attachmentRows.Err()
}

func parseDBTime(value string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func addICloudAccount(ctx context.Context, sqldb *sql.DB, vault *credentialVault, login, email, password string) (int64, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	password = strings.TrimSpace(password)
	if email == "" || !strings.Contains(email, "@") {
		return 0, fmt.Errorf("enter your iCloud Mail address")
	}
	if password == "" {
		return 0, fmt.Errorf("enter an Apple app-specific password")
	}
	ciphertext, nonce, err := vault.Seal(accountCredential{AppPassword: password})
	if err != nil {
		return 0, err
	}
	result, err := sqldb.ExecContext(ctx, `INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce, status, status_detail)
		VALUES (?, 'icloud', ?, ?, ?, 'pending', 'Ready for first sync')`, login, email, ciphertext, nonce)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		return 0, fmt.Errorf("that account is already connected")
	}
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
