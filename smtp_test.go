package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildMessageOmitsBccHeader(t *testing.T) {
	d := draft{
		To: []string{"Alice <alice@example.com>"}, Cc: []string{"cc@example.com"},
		Bcc: []string{"hidden@example.com"}, Subject: "Hello ✓", Body: "first\nsecond",
	}
	message, recipients, err := buildMessage("sender@example.com", d)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(message)
	if strings.Contains(raw, "Bcc:") || strings.Contains(raw, "hidden@example.com") {
		t.Fatal("serialized message leaked Bcc recipient")
	}
	if len(recipients) != 3 || recipients[2] != "hidden@example.com" {
		t.Fatalf("recipients = %#v", recipients)
	}
	if !strings.Contains(raw, "first\r\nsecond") {
		t.Fatal("body was not normalized to CRLF")
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	_, _, err := buildMessage("sender@example.com", draft{
		To: []string{"victim@example.com"}, Subject: "hello\r\nBcc: attacker@example.com",
	})
	if err == nil {
		t.Fatal("expected subject header injection to be rejected")
	}
}

func TestBuildMessageIncludesStoredAttachment(t *testing.T) {
	t.Setenv("BESPOKE_DATA", t.TempDir())
	cacheKey := strings.Repeat("ab", 24)
	if err := os.MkdirAll(mailAttachmentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("attachment payload")
	if err := os.WriteFile(filepath.Join(mailAttachmentDir(), cacheKey), want, 0o600); err != nil {
		t.Fatal(err)
	}
	message, _, err := buildMessageWithAttachments("sender@example.com", draft{
		To: []string{"recipient@example.com"}, Subject: "With file", Body: "hello",
	}, []draftAttachment{{Filename: "report.txt", ContentType: "text/plain", CacheKey: cacheKey}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	if _, err := reader.NextPart(); err != nil { // text body
		t.Fatal(err)
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
	if err != nil {
		t.Fatal(err)
	}
	if part.FileName() != "report.txt" || !bytes.Equal(got, want) {
		t.Fatalf("attachment filename=%q body=%q", part.FileName(), got)
	}
}

func TestParseAddressInput(t *testing.T) {
	got, err := parseAddressInput("Alice <alice@example.com>, bob@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "<bob@example.com>" {
		t.Fatalf("parseAddressInput() = %#v", got)
	}
	if _, err := parseAddressInput("", true); err == nil {
		t.Fatal("expected missing recipient error")
	}
}
