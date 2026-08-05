package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestDraftAttachmentStorageIsScopedAndFeedsMIME(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO drafts (login, subject, body) VALUES ('owner', 'Files', 'See attached')`)
	if err != nil {
		t.Fatal(err)
	}
	draftID, _ := result.LastInsertId()
	if err := addDraftAttachment(context.Background(), sqldb, "intruder", draftID, "private.txt", "text/plain", strings.NewReader("secret")); err == nil {
		t.Fatal("expected cross-user attachment upload to fail")
	}
	if err := addDraftAttachment(context.Background(), sqldb, "owner", draftID, "private.txt", "text/plain", strings.NewReader("secret")); err != nil {
		t.Fatal(err)
	}
	d, err := loadDraft(context.Background(), sqldb, "owner", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(d.Attachments))
	}
	stored, err := os.ReadFile(mailAttachmentDir() + "/" + d.Attachments[0].CacheKey)
	if err != nil || !bytes.Equal(stored, []byte("secret")) {
		t.Fatalf("stored attachment = %q, err %v", stored, err)
	}
	d.To = []string{"reader@example.com"}
	message, _, err := buildMessageWithAttachments("owner@icloud.com", d, d.Attachments)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"multipart/mixed", "private.txt", "c2VjcmV0"} {
		if !bytes.Contains(message, []byte(want)) {
			t.Fatalf("MIME message missing %q", want)
		}
	}
	if err := removeDraftAttachment(context.Background(), sqldb, "owner", draftID, d.Attachments[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mailAttachmentDir() + "/" + d.Attachments[0].CacheKey); !os.IsNotExist(err) {
		t.Fatalf("attachment file still exists: %v", err)
	}
}

func TestSentDraftCannotBeEditedOrGivenAttachments(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO drafts (login, subject, body, status)
		VALUES ('owner', 'Original', 'body', 'sent')`)
	if err != nil {
		t.Fatal(err)
	}
	draftID, _ := result.LastInsertId()
	if _, err := saveDraft(context.Background(), sqldb, "owner", draftID, 0, "", "", "", "Changed", "body"); err == nil {
		t.Fatal("expected editing a sent draft to fail")
	}
	if err := addDraftAttachment(context.Background(), sqldb, "owner", draftID, "late.txt", "text/plain", strings.NewReader("late")); err == nil {
		t.Fatal("expected attaching to a sent draft to fail")
	}
}

func TestDeleteDraftIsScopedAndCleansAttachment(t *testing.T) {
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	result, err := sqldb.Exec(`INSERT INTO drafts (login, subject) VALUES ('owner', 'Disposable')`)
	if err != nil {
		t.Fatal(err)
	}
	draftID, _ := result.LastInsertId()
	if err := addDraftAttachment(context.Background(), sqldb, "owner", draftID, "note.txt", "text/plain", strings.NewReader("note")); err != nil {
		t.Fatal(err)
	}
	d, err := loadDraft(context.Background(), sqldb, "owner", draftID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := draftAttachmentPath(d.Attachments[0].CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteDraft(context.Background(), sqldb, "intruder", draftID); err == nil {
		t.Fatal("expected cross-user deletion to fail")
	}
	if err := deleteDraft(context.Background(), sqldb, "owner", draftID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("attachment file still exists: %v", err)
	}
	if _, err := loadDraft(context.Background(), sqldb, "owner", draftID); err == nil {
		t.Fatal("deleted draft still exists")
	}
}
