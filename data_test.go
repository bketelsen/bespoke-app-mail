package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"testing"

	bespokedb "github.com/bketelsen/bespoke/pkg/db"
)

func TestMigrationCreatesMailSchema(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()

	for _, table := range []string{"accounts", "mailboxes", "messages", "message_mailboxes", "drafts", "pending_operations", "oauth_states"} {
		var found string
		if err := sqldb.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&found); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
}

func TestAllInboxesAggregatesConnectedAccounts(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()

	wantMailbox := make(map[int64]bool)
	for i, provider := range []string{"gmail", "icloud"} {
		accountResult, err := sqldb.Exec(`INSERT INTO accounts
			(login, provider, email, credential_ciphertext, credential_nonce)
			VALUES ('owner', ?, ?, X'01', X'02')`, provider, provider+"@example.com")
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := accountResult.LastInsertId()
		mailboxResult, err := sqldb.Exec(`INSERT INTO mailboxes
			(account_id, remote_name, display_name, role) VALUES (?, 'INBOX', 'Inbox', 'inbox')`, accountID)
		if err != nil {
			t.Fatal(err)
		}
		mailboxID, _ := mailboxResult.LastInsertId()
		wantMailbox[mailboxID] = true
		messageResult, err := sqldb.Exec(`INSERT INTO messages
			(account_id, remote_key, subject, received_at) VALUES (?, ?, ?, datetime('now', ?))`,
			accountID, fmt.Sprintf("inbox:%d", i), provider, fmt.Sprintf("+%d seconds", i))
		if err != nil {
			t.Fatal(err)
		}
		messageID, _ := messageResult.LastInsertId()
		if _, err := sqldb.Exec(`INSERT INTO message_mailboxes (message_id, mailbox_id, remote_uid)
			VALUES (?, ?, 1)`, messageID, mailboxID); err != nil {
			t.Fatal(err)
		}
	}

	workspace, err := loadWorkspace(context.Background(), sqldb, "owner", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Messages) != 2 {
		t.Fatalf("All Inboxes returned %d messages, want 2", len(workspace.Messages))
	}
	for _, message := range workspace.Messages {
		if !wantMailbox[message.MailboxID] {
			t.Errorf("message mailbox = %d, want one of the account inboxes", message.MailboxID)
		}
	}
}

func TestDisconnectMailAccountCascadesCacheAndPreservesDraft(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	accountResult, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'icloud', 'owner@icloud.com', X'01', X'02')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	mailboxResult, err := sqldb.Exec(`INSERT INTO mailboxes
		(account_id, remote_name, display_name, role) VALUES (?, 'INBOX', 'Inbox', 'inbox')`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	mailboxID, _ := mailboxResult.LastInsertId()
	messageResult, err := sqldb.Exec(`INSERT INTO messages
		(account_id, remote_key, received_at) VALUES (?, 'inbox:1', datetime('now'))`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := messageResult.LastInsertId()
	if _, err := sqldb.Exec(`INSERT INTO message_mailboxes (message_id, mailbox_id, remote_uid) VALUES (?, ?, 1)`, messageID, mailboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.Exec(`INSERT INTO pending_operations (account_id, message_id, kind) VALUES (?, ?, 'star')`, accountID, messageID); err != nil {
		t.Fatal(err)
	}
	draftResult, err := sqldb.Exec(`INSERT INTO drafts (login, account_id, subject) VALUES ('owner', ?, 'Keep me')`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, _ := draftResult.LastInsertId()

	if _, err := disconnectMailAccount(context.Background(), sqldb, "owner", accountID, false); err == nil {
		t.Fatal("expected explicit confirmation to be required")
	}
	if _, err := disconnectMailAccount(context.Background(), sqldb, "intruder", accountID, true); err == nil {
		t.Fatal("expected cross-user disconnect to fail")
	}
	removed, err := disconnectMailAccount(context.Background(), sqldb, "owner", accountID, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Provider != "icloud" || removed.Email != "owner@icloud.com" {
		t.Fatalf("removed = %+v", removed)
	}
	for _, table := range []string{"accounts", "mailboxes", "messages", "message_mailboxes", "pending_operations"} {
		var count int
		if err := sqldb.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows", table, count)
		}
	}
	var draftAccount sql.NullInt64
	if err := sqldb.QueryRow("SELECT account_id FROM drafts WHERE id=?", draftID).Scan(&draftAccount); err != nil {
		t.Fatal(err)
	}
	if draftAccount.Valid {
		t.Fatalf("draft account = %d, want NULL", draftAccount.Int64)
	}
}

func openTestMailDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("BESPOKE_DATA", t.TempDir())
	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sqldb, err := bespokedb.Open("mail-test", migrations)
	if err != nil {
		t.Fatal(err)
	}
	return sqldb
}
