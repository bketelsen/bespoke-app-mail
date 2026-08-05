package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestMailboxRole(t *testing.T) {
	tests := []struct {
		name  string
		attrs []imap.MailboxAttr
		want  string
	}{
		{name: "INBOX", want: "inbox"},
		{name: "Sent Messages", attrs: []imap.MailboxAttr{imap.MailboxAttrSent}, want: "sent"},
		{name: "All Mail", attrs: []imap.MailboxAttr{imap.MailboxAttrAll}, want: "archive"},
		{name: "Deleted", attrs: []imap.MailboxAttr{imap.MailboxAttrTrash}, want: "trash"},
		{name: "Receipts", want: "folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mailboxRole("gmail", tt.name, tt.attrs); got != tt.want {
				t.Fatalf("mailboxRole() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMailboxAttributesAreCaseInsensitive(t *testing.T) {
	attrs := []imap.MailboxAttr{"\\NoSelect", "\\SENT"}
	if !mailboxHasAttr(attrs, imap.MailboxAttrNoSelect) {
		t.Fatal("expected Gmail-style \\NoSelect to match")
	}
	if role := mailboxRole("gmail", "[Gmail]/Sent Mail", attrs); role != "sent" {
		t.Fatalf("mailboxRole() = %q, want sent", role)
	}
}

func TestICloudMailboxRolesWithoutSpecialUseAttributes(t *testing.T) {
	tests := map[string]string{
		"Archive": "archive", "Sent Messages": "sent", "Drafts": "drafts",
		"Deleted Messages": "trash", "Junk": "spam",
	}
	for name, want := range tests {
		if got := mailboxRole("icloud", name, nil); got != want {
			t.Errorf("mailboxRole(icloud, %q) = %q, want %q", name, got, want)
		}
		if got := mailboxRole("gmail", name, nil); got != "folder" {
			t.Errorf("mailboxRole(gmail, %q) = %q, want folder", name, got)
		}
	}
}

func TestStoreMailboxListingRemovesStaleNoSelectContainer(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'gmail', 'owner@gmail.com', X'01', X'02')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := sqldb.Exec(`INSERT INTO mailboxes
		(account_id, remote_name, display_name, role) VALUES (?, '[Gmail]', '[Gmail]', 'folder')`, accountID); err != nil {
		t.Fatal(err)
	}
	syncer := &mailSynchronizer{db: sqldb}
	selectable, err := syncer.storeMailboxListing(context.Background(), syncAccount{ID: accountID},
		&imap.ListData{Mailbox: "[Gmail]", Attrs: []imap.MailboxAttr{"\\NoSelect"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if selectable {
		t.Fatal("non-selectable Gmail container reported selectable")
	}
	var count int
	if err := sqldb.QueryRow(`SELECT count(*) FROM mailboxes WHERE account_id=?`, accountID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale mailbox count = %d, want 0", count)
	}
}

func TestGmailNamespaceContainerIsNotSelectable(t *testing.T) {
	if isSelectableMailbox("gmail", &imap.ListData{Mailbox: "[Gmail]"}) {
		t.Fatal("Gmail namespace container reported selectable without a flag")
	}
	if !isSelectableMailbox("gmail", &imap.ListData{Mailbox: "[Gmail]/Sent Mail"}) {
		t.Fatal("Gmail special-use child reported non-selectable")
	}
	if !isSelectableMailbox("icloud", &imap.ListData{Mailbox: "[Gmail]"}) {
		t.Fatal("provider-specific rule affected iCloud")
	}
}

func TestXOAUTH2InitialResponse(t *testing.T) {
	client := &xoauth2Client{username: "person@gmail.com", token: "token-value"}
	mechanism, response, err := client.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mechanism != "XOAUTH2" {
		t.Fatalf("mechanism = %q", mechanism)
	}
	want := "user=person@gmail.com\x01auth=Bearer token-value\x01\x01"
	if string(response) != want {
		t.Fatalf("response = %q, want %q", response, want)
	}
}

func TestPreferredTextPartSkipsAttachment(t *testing.T) {
	structure := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 40,
			Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment"}}},
		&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 100},
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 55},
		}},
	}}
	path, size, ok := preferredTextPart(structure)
	if !ok || size != 55 || len(path) != 2 || path[0] != 2 || path[1] != 2 {
		t.Fatalf("preferredTextPart() = %v, %d, %v", path, size, ok)
	}
	htmlPath, htmlSize, htmlOK := preferredHTMLPart(structure)
	if !htmlOK || htmlSize != 100 || len(htmlPath) != 2 || htmlPath[0] != 2 || htmlPath[1] != 1 {
		t.Fatalf("preferredHTMLPart() = %v, %d, %v", htmlPath, htmlSize, htmlOK)
	}
}

func TestSanitizeMessageHTMLRemovesActiveContent(t *testing.T) {
	got := string(sanitizeMessageHTML(`<p>Hello <strong>mail</strong></p><script>alert(1)</script><img src="x" onerror="alert(2)">`))
	if !strings.Contains(got, "<strong>mail</strong>") {
		t.Fatalf("safe formatting was removed: %q", got)
	}
	for _, unsafe := range []string{"<script", "onerror", "alert(1)"} {
		if strings.Contains(strings.ToLower(got), unsafe) {
			t.Fatalf("sanitized HTML contains %q: %q", unsafe, got)
		}
	}
}

func TestHTMLMessageUsesLightColorScheme(t *testing.T) {
	if !strings.Contains(htmlMessageDocumentStart, `content="only light"`) || !strings.Contains(htmlMessageDocumentStart, "color-scheme:only light") {
		t.Fatal("HTML message shell does not force a light color scheme")
	}
}

func TestMessagePreview(t *testing.T) {
	got := messagePreview("  hello\n\nworld\tfrom mail  ")
	if got != "hello world from mail" {
		t.Fatalf("messagePreview() = %q", got)
	}
}

func TestCacheAttachmentMetadata(t *testing.T) {
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
	structure := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 20},
		&imap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 1234,
			Params: map[string]string{"name": "receipt.pdf"}},
	}}
	syncer := &mailSynchronizer{db: sqldb}
	if err := syncer.cacheAttachmentMetadata(context.Background(), messageID, structure); err != nil {
		t.Fatal(err)
	}
	var path, filename string
	var size int
	if err := sqldb.QueryRow("SELECT part_path, filename, size_bytes FROM attachments WHERE message_id=?", messageID).
		Scan(&path, &filename, &size); err != nil {
		t.Fatal(err)
	}
	if path != "2" || filename != "receipt.pdf" || size != 1234 {
		t.Fatalf("attachment = %q %q %d", path, filename, size)
	}
}

func TestReconcileMailboxWindowRemovesOnlyStaleOrphans(t *testing.T) {
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
	for _, uid := range []int{10, 11} {
		result, insertErr := sqldb.Exec(`INSERT INTO messages
			(account_id, remote_key, received_at) VALUES (?, ?, datetime('now'))`, accountID, fmt.Sprintf("inbox:%d", uid))
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		messageID, _ := result.LastInsertId()
		if _, insertErr = sqldb.Exec(`INSERT INTO message_mailboxes (message_id, mailbox_id, remote_uid)
			VALUES (?, ?, ?)`, messageID, mailboxID, uid); insertErr != nil {
			t.Fatal(insertErr)
		}
	}
	syncer := &mailSynchronizer{db: sqldb}
	if err := syncer.reconcileMailboxWindow(context.Background(), mailboxID, map[uint32]struct{}{11: {}}); err != nil {
		t.Fatal(err)
	}
	var messages, memberships int
	if err := sqldb.QueryRow("SELECT count(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := sqldb.QueryRow("SELECT count(*) FROM message_mailboxes").Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || memberships != 1 {
		t.Fatalf("messages=%d memberships=%d, want one retained message", messages, memberships)
	}
}
