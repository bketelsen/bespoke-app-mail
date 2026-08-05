package main

import (
	"context"
	"testing"
)

func TestLoadDraftDecodesEqualEmptyAddressLists(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO drafts (login, to_json, cc_json, bcc_json)
		VALUES ('owner', '[]', '[]', '[]')`)
	if err != nil {
		t.Fatal(err)
	}
	draftID, _ := result.LastInsertId()
	d, err := loadDraft(context.Background(), sqldb, "owner", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if d.To == nil || d.Cc == nil || d.Bcc == nil {
		t.Fatalf("address lists must be empty arrays, got to=%v cc=%v bcc=%v", d.To, d.Cc, d.Bcc)
	}
}

func TestCreateReplyAllUsesReplyToAndKeepsCarbonCopies(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	accountResult, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'icloud', 'owner@icloud.com', X'01', X'02')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	messageResult, err := sqldb.Exec(`INSERT INTO messages
		(account_id, remote_key, subject, received_at, text_body)
		VALUES (?, 'inbox:42', 'A topic', datetime('now'), 'hello')`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := messageResult.LastInsertId()
	for _, address := range []struct{ kind, value string }{
		{"from", "sender@example.com"},
		{"reply-to", "replies@example.com"},
		{"to", "owner@icloud.com"},
		{"to", "teammate@example.com"},
		{"cc", "observer@example.com"},
	} {
		if _, err := sqldb.Exec(`INSERT INTO addresses (message_id, kind, position, address)
			VALUES (?, ?, 0, ?)`, messageID, address.kind, address.value); err != nil {
			t.Fatal(err)
		}
	}
	draftID, err := createResponseDraft(context.Background(), sqldb, "owner", messageID, "reply-all")
	if err != nil {
		t.Fatal(err)
	}
	d, err := loadDraft(context.Background(), sqldb, "owner", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.To) != 1 || !containsAddress(d.To, "replies@example.com") {
		t.Fatalf("To = %v, want Reply-To only", d.To)
	}
	if len(d.Cc) != 2 || !containsAddress(d.Cc, "teammate@example.com") || !containsAddress(d.Cc, "observer@example.com") {
		t.Fatalf("Cc = %v, want original non-owner recipients", d.Cc)
	}
}
