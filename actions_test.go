package main

import (
	"context"
	"testing"
)

func TestQueueMessageActionIsUserScopedAndOptimistic(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'icloud', 'owner@icloud.com', X'01', X'02')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	result, err = sqldb.Exec(`INSERT INTO messages
		(account_id, remote_key, received_at) VALUES (?, '1:1', datetime('now'))`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := result.LastInsertId()
	mailboxResult, err := sqldb.Exec(`INSERT INTO mailboxes
		(account_id, remote_name, display_name, role) VALUES (?, 'INBOX', 'Inbox', 'inbox')`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	mailboxID, _ := mailboxResult.LastInsertId()
	if _, err := sqldb.Exec(`INSERT INTO message_mailboxes (message_id, mailbox_id, remote_uid)
		VALUES (?, ?, 1)`, messageID, mailboxID); err != nil {
		t.Fatal(err)
	}

	queuedAccount, err := queueMessageAction(context.Background(), sqldb, "owner", messageID, "mark-read")
	if err != nil {
		t.Fatal(err)
	}
	if queuedAccount != accountID {
		t.Fatalf("account = %d, want %d", queuedAccount, accountID)
	}
	var isRead, pending int
	if err := sqldb.QueryRow("SELECT is_read FROM messages WHERE id=?", messageID).Scan(&isRead); err != nil {
		t.Fatal(err)
	}
	if err := sqldb.QueryRow("SELECT count(*) FROM pending_operations WHERE message_id=? AND kind='mark-read'", messageID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if isRead != 1 || pending != 1 {
		t.Fatalf("is_read=%d pending=%d", isRead, pending)
	}
	if _, err := queueMessageAction(context.Background(), sqldb, "someone-else", messageID, "star"); err == nil {
		t.Fatal("expected cross-user action to fail")
	}
	if _, err := queueMessageAction(context.Background(), sqldb, "owner", messageID, "archive"); err != nil {
		t.Fatal(err)
	}
	workspace, err := loadWorkspace(context.Background(), sqldb, "owner", mailboxID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Messages) != 0 {
		t.Fatalf("archived message remains visible while provider operation is pending: %d messages", len(workspace.Messages))
	}
}

func TestSafeMailReturnPath(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"/mailboxes/2", "/mailboxes/2"},
		{"/messages/3?view=full", "/messages/3?view=full"},
		{"https://example.com/steal", "/"},
		{"//example.com/steal", "/"},
		{"", "/"},
	} {
		if got := safeMailReturnPath(test.input); got != test.want {
			t.Errorf("safeMailReturnPath(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestQueueMessageActionsIsAtomicAndDeduplicatesAccounts(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	accountResult, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'icloud', 'owner@icloud.com', X'01', X'02')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	var messageIDs []int64
	for _, key := range []string{"1:1", "1:2"} {
		result, insertErr := sqldb.Exec(`INSERT INTO messages
			(account_id, remote_key, received_at) VALUES (?, ?, datetime('now'))`, accountID, key)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		id, _ := result.LastInsertId()
		messageIDs = append(messageIDs, id)
	}
	accountIDs, err := queueMessageActions(context.Background(), sqldb, "owner",
		[]int64{messageIDs[1], messageIDs[0], messageIDs[1]}, "star")
	if err != nil {
		t.Fatal(err)
	}
	if len(accountIDs) != 1 || accountIDs[0] != accountID {
		t.Fatalf("accounts = %v, want [%d]", accountIDs, accountID)
	}
	var starred, pending int
	if err := sqldb.QueryRow("SELECT count(*) FROM messages WHERE is_starred=1").Scan(&starred); err != nil {
		t.Fatal(err)
	}
	if err := sqldb.QueryRow("SELECT count(*) FROM pending_operations WHERE kind='star'").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if starred != 2 || pending != 2 {
		t.Fatalf("starred=%d pending=%d, want 2 each", starred, pending)
	}

	if _, err := queueMessageActions(context.Background(), sqldb, "owner", []int64{messageIDs[0], 9999}, "trash"); err == nil {
		t.Fatal("expected unavailable message to abort batch")
	}
	if err := sqldb.QueryRow("SELECT count(*) FROM pending_operations WHERE kind='trash'").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("partial trash operations = %d, want 0", pending)
	}
}
