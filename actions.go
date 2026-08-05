package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var messageActions = map[string]string{
	"mark-read": "mark-read", "mark-unread": "mark-unread",
	"star": "star", "unstar": "unstar", "archive": "archive", "trash": "trash",
}

func queueMessageActions(ctx context.Context, sqldb *sql.DB, login string, messageIDs []int64, action string) ([]int64, error) {
	kind, ok := messageActions[action]
	if !ok {
		return nil, errors.New("unsupported message action")
	}
	messageIDs = append([]int64(nil), messageIDs...)
	slices.Sort(messageIDs)
	messageIDs = slices.Compact(messageIDs)
	if len(messageIDs) == 0 || len(messageIDs) > 50 {
		return nil, errors.New("provide between 1 and 50 message IDs")
	}
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	accountSet := make(map[int64]struct{})
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return nil, errors.New("message IDs must be positive")
		}
		var accountID int64
		if err := tx.QueryRowContext(ctx, `SELECT m.account_id FROM messages m JOIN accounts a ON a.id=m.account_id
			WHERE m.id=? AND a.login=?`, messageID, login).Scan(&accountID); err != nil {
			return nil, fmt.Errorf("message #%d is unavailable", messageID)
		}
		accountSet[accountID] = struct{}{}
		localUpdate := ""
		switch kind {
		case "mark-read":
			localUpdate = "UPDATE messages SET is_read=1, updated_at=datetime('now') WHERE id=?"
		case "mark-unread":
			localUpdate = "UPDATE messages SET is_read=0, updated_at=datetime('now') WHERE id=?"
		case "star":
			localUpdate = "UPDATE messages SET is_starred=1, updated_at=datetime('now') WHERE id=?"
		case "unstar":
			localUpdate = "UPDATE messages SET is_starred=0, updated_at=datetime('now') WHERE id=?"
		}
		if localUpdate != "" {
			if _, err := tx.ExecContext(ctx, localUpdate, messageID); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pending_operations
			(account_id, message_id, kind) VALUES (?, ?, ?)`, accountID, messageID, kind); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	accountIDs := make([]int64, 0, len(accountSet))
	for accountID := range accountSet {
		accountIDs = append(accountIDs, accountID)
	}
	slices.Sort(accountIDs)
	return accountIDs, nil
}

type pendingMessageOperation struct {
	id, messageID int64
	kind, mailbox string
	uid           uint32
}

func queueMessageAction(ctx context.Context, sqldb *sql.DB, login string, messageID int64, action string) (int64, error) {
	kind, ok := messageActions[action]
	if !ok {
		return 0, errors.New("unsupported message action")
	}
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var accountID int64
	if err := tx.QueryRowContext(ctx, `SELECT m.account_id FROM messages m JOIN accounts a ON a.id=m.account_id
		WHERE m.id=? AND a.login=?`, messageID, login).Scan(&accountID); err != nil {
		return 0, err
	}
	localUpdate := ""
	switch kind {
	case "mark-read":
		localUpdate = "UPDATE messages SET is_read=1, updated_at=datetime('now') WHERE id=?"
	case "mark-unread":
		localUpdate = "UPDATE messages SET is_read=0, updated_at=datetime('now') WHERE id=?"
	case "star":
		localUpdate = "UPDATE messages SET is_starred=1, updated_at=datetime('now') WHERE id=?"
	case "unstar":
		localUpdate = "UPDATE messages SET is_starred=0, updated_at=datetime('now') WHERE id=?"
	}
	if localUpdate != "" {
		if _, err := tx.ExecContext(ctx, localUpdate, messageID); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_operations
		(account_id, message_id, kind) VALUES (?, ?, ?)`, accountID, messageID, kind); err != nil {
		return 0, err
	}
	return accountID, tx.Commit()
}

func (s *mailSynchronizer) applyPending(ctx context.Context, client *imapclient.Client, a syncAccount) error {
	rows, err := s.db.QueryContext(ctx, `SELECT po.id, po.message_id, po.kind, mb.remote_name, mm.remote_uid
		FROM pending_operations po JOIN messages m ON m.id=po.message_id
		JOIN message_mailboxes mm ON mm.message_id=m.id
		JOIN mailboxes mb ON mb.id=mm.mailbox_id
		WHERE po.account_id=? AND po.status IN ('pending','failed') AND po.next_attempt_at<=datetime('now')
		AND mm.mailbox_id=(SELECT mm2.mailbox_id FROM message_mailboxes mm2
			JOIN mailboxes mb2 ON mb2.id=mm2.mailbox_id WHERE mm2.message_id=m.id
			ORDER BY CASE mb2.role WHEN 'inbox' THEN 0 ELSE 1 END, mm2.mailbox_id LIMIT 1)
		ORDER BY po.id LIMIT 50`, a.ID)
	if err != nil {
		return err
	}
	var operations []pendingMessageOperation
	for rows.Next() {
		var op pendingMessageOperation
		if err := rows.Scan(&op.id, &op.messageID, &op.kind, &op.mailbox, &op.uid); err != nil {
			_ = rows.Close()
			return err
		}
		operations = append(operations, op)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, op := range operations {
		if _, err := s.db.ExecContext(ctx, `UPDATE pending_operations SET status='running',
			attempts=attempts+1, updated_at=datetime('now') WHERE id=?`, op.id); err != nil {
			return err
		}
		_, opErr := client.Select(op.mailbox, nil).Wait()
		if opErr == nil {
			opErr = s.applyRemoteOperation(ctx, client, a.ID, op)
		}
		if opErr != nil {
			_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE pending_operations SET status='failed',
				last_error=?, next_attempt_at=datetime('now','+5 minutes'), updated_at=datetime('now') WHERE id=?`,
				truncateError(opErr), op.id)
			return fmt.Errorf("apply %s: %w", op.kind, opErr)
		}
		if op.kind == "archive" || op.kind == "trash" {
			// The next sync will rediscover the message in its destination if that
			// mailbox is cached. Removing this local copy avoids stale inbox state.
			if _, err := s.db.ExecContext(ctx, "DELETE FROM messages WHERE id=?", op.messageID); err != nil {
				return err
			}
		} else if _, err := s.db.ExecContext(ctx, "DELETE FROM pending_operations WHERE id=?", op.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *mailSynchronizer) applyRemoteOperation(ctx context.Context, client *imapclient.Client, accountID int64, op pendingMessageOperation) error {
	uidSet := imap.UIDSetNum(imap.UID(op.uid))
	var flag imap.Flag
	var flagOp imap.StoreFlagsOp
	switch op.kind {
	case "mark-read":
		flag, flagOp = imap.FlagSeen, imap.StoreFlagsAdd
	case "mark-unread":
		flag, flagOp = imap.FlagSeen, imap.StoreFlagsDel
	case "star":
		flag, flagOp = imap.FlagFlagged, imap.StoreFlagsAdd
	case "unstar":
		flag, flagOp = imap.FlagFlagged, imap.StoreFlagsDel
	case "archive", "trash":
		role := op.kind
		var destination string
		if err := s.db.QueryRowContext(ctx, `SELECT remote_name FROM mailboxes
			WHERE account_id=? AND role=? ORDER BY position LIMIT 1`, accountID, role).Scan(&destination); err != nil {
			return fmt.Errorf("%s mailbox unavailable", role)
		}
		_, err := client.Move(uidSet, destination).Wait()
		return err
	default:
		return errors.New("unsupported pending operation")
	}
	return client.Store(uidSet, &imap.StoreFlags{Op: flagOp, Silent: true, Flags: []imap.Flag{flag}}, nil).Close()
}
