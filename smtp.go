package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

func (s *mailSynchronizer) SendDraft(ctx context.Context, login string, draftID int64) error {
	d, err := loadDraft(ctx, s.db, login, draftID)
	if err != nil {
		return err
	}
	if d.Status == "sent" {
		return errors.New("draft was already sent")
	}
	if d.AccountID == 0 {
		return errors.New("choose a sending account")
	}
	if len(d.To)+len(d.Cc)+len(d.Bcc) == 0 {
		return errors.New("at least one recipient is required")
	}
	var a syncAccount
	err = s.db.QueryRowContext(ctx, `SELECT id, login, provider, email,
		credential_ciphertext, credential_nonce FROM accounts WHERE id=? AND login=? AND status!='disabled'`,
		d.AccountID, login).Scan(&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce)
	if err != nil {
		return errors.New("sending account is unavailable")
	}
	credential, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return err
	}
	if a.Provider == "gmail" {
		credential, err = s.freshGoogleCredential(ctx, a, credential)
		if err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE drafts SET status='sending', status_detail='',
		updated_at=datetime('now') WHERE id=? AND login=?`, draftID, login); err != nil {
		return err
	}
	message, recipients, err := buildMessageWithAttachments(a.Email, d, d.Attachments)
	if err == nil {
		err = sendSMTP(ctx, a, credential, recipients, message)
	}
	if err != nil {
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE drafts SET status='error',
			status_detail=?, updated_at=datetime('now') WHERE id=? AND login=?`, truncateError(err), draftID, login)
		return err
	}
	detail := ""
	if a.Provider == "icloud" {
		if appendErr := s.appendSentCopy(ctx, a, credential, message); appendErr != nil {
			// SMTP already accepted the message. Keep the terminal sent state so a
			// retry cannot deliver a duplicate, but preserve an actionable warning.
			detail = "Delivered, but the Sent copy could not be filed: " + truncateError(appendErr)
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE drafts SET status='sent', status_detail=?,
		updated_at=datetime('now') WHERE id=? AND login=?`, detail, draftID, login)
	return err
}

func (s *mailSynchronizer) appendSentCopy(ctx context.Context, a syncAccount, credential accountCredential, message []byte) error {
	var mailbox string
	if err := s.db.QueryRowContext(ctx, `SELECT remote_name FROM mailboxes
		WHERE account_id=? AND role='sent' ORDER BY position, id LIMIT 1`, a.ID).Scan(&mailbox); err != nil {
		return errors.New("sent mailbox is unavailable; sync the account and try again")
	}
	client, err := openIMAP(ctx, a, credential)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	command := client.Append(mailbox, int64(len(message)), &imap.AppendOptions{
		Flags: []imap.Flag{imap.FlagSeen}, Time: time.Now(),
	})
	if _, err := command.Write(message); err != nil {
		_ = command.Close()
		return fmt.Errorf("write Sent copy: %w", err)
	}
	if err := command.Close(); err != nil {
		return fmt.Errorf("close Sent copy: %w", err)
	}
	if _, err := command.Wait(); err != nil {
		return fmt.Errorf("file Sent copy: %w", err)
	}
	return nil
}

func buildMessage(from string, d draft) ([]byte, []string, error) {
	return buildMessageWithAttachments(from, d, nil)
}

func buildMessageWithAttachments(from string, d draft, attachments []draftAttachment) ([]byte, []string, error) {
	if strings.ContainsAny(d.Subject, "\r\n") {
		return nil, nil, errors.New("subject contains an invalid line break")
	}
	all := append(append(append([]string{}, d.To...), d.Cc...), d.Bcc...)
	recipients := make([]string, 0, len(all))
	for _, value := range all {
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, nil, errors.New("draft contains an invalid recipient")
		}
		recipients = append(recipients, address.Address)
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, err
	}
	domain := "bespoke.local"
	if parts := strings.SplitN(from, "@", 2); len(parts) == 2 {
		domain = parts[1]
	}
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\n", from)
	fmt.Fprintf(&message, "To: %s\r\n", strings.Join(d.To, ", "))
	if len(d.Cc) > 0 {
		fmt.Fprintf(&message, "Cc: %s\r\n", strings.Join(d.Cc, ", "))
	}
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&message, "Message-ID: <%s@%s>\r\n", base64.RawURLEncoding.EncodeToString(random), domain)
	fmt.Fprintf(&message, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", d.Subject))
	message.WriteString("MIME-Version: 1.0\r\n")
	if len(attachments) == 0 {
		message.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
		message.WriteString(strings.ReplaceAll(strings.ReplaceAll(d.Body, "\r\n", "\n"), "\n", "\r\n"))
		return message.Bytes(), recipients, nil
	}
	mixed := multipart.NewWriter(&message)
	message.WriteString("Content-Type: " + mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": mixed.Boundary()}) + "\r\n\r\n")
	textHeader := make(textproto.MIMEHeader)
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textHeader.Set("Content-Transfer-Encoding", "8bit")
	textPart, err := mixed.CreatePart(textHeader)
	if err != nil {
		return nil, nil, err
	}
	if _, err := textPart.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(d.Body, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return nil, nil, err
	}
	for _, attachment := range attachments {
		path, err := draftAttachmentPath(attachment.CacheKey)
		if err != nil {
			return nil, nil, fmt.Errorf("open attachment %q: %w", attachment.Filename, err)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open attachment %q: %w", attachment.Filename, err)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Type", attachment.ContentType)
		header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Filename}))
		header.Set("Content-Transfer-Encoding", "base64")
		part, partErr := mixed.CreatePart(header)
		if partErr == nil {
			encoder := base64.NewEncoder(base64.StdEncoding, &mimeLineWriter{writer: part})
			_, partErr = io.Copy(encoder, file)
			if closeErr := encoder.Close(); partErr == nil {
				partErr = closeErr
			}
		}
		_ = file.Close()
		if partErr != nil {
			return nil, nil, fmt.Errorf("encode attachment %q: %w", attachment.Filename, partErr)
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, nil, err
	}
	return message.Bytes(), recipients, nil
}

type mimeLineWriter struct {
	writer io.Writer
	column int
}

func (w *mimeLineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		room := 76 - w.column
		count := min(room, len(p))
		if _, err := w.writer.Write(p[:count]); err != nil {
			return written, err
		}
		written += count
		w.column += count
		p = p[count:]
		if w.column == 76 {
			if _, err := io.WriteString(w.writer, "\r\n"); err != nil {
				return written, err
			}
			w.column = 0
		}
	}
	return written, nil
}

func sendSMTP(ctx context.Context, a syncAccount, credential accountCredential, recipients []string, message []byte) error {
	client, err := openSMTP(ctx, a, credential)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Mail(a.Email); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func openSMTP(ctx context.Context, a syncAccount, credential accountCredential) (*smtp.Client, error) {
	var client *smtp.Client
	var auth smtp.Auth
	var connection net.Conn
	switch a.Provider {
	case "gmail":
		host := "smtp.gmail.com"
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", host+":465", &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("connect to Gmail SMTP: %w", err)
		}
		connection = conn
		client, err = smtp.NewClient(conn, host)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		auth = &smtpXOAUTH2{username: a.Email, token: credential.AccessToken}
	case "icloud":
		host := "smtp.mail.me.com"
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", host+":587")
		if err != nil {
			return nil, fmt.Errorf("connect to iCloud SMTP: %w", err)
		}
		connection = conn
		client, err = smtp.NewClient(conn, host)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("secure iCloud SMTP: %w", err)
		}
		auth = smtp.PlainAuth("", a.Email, credential.AppPassword, host)
	default:
		return nil, fmt.Errorf("unsupported provider %q", a.Provider)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := client.Auth(auth); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("authenticate SMTP: %w", err)
	}
	return client, nil
}

type smtpXOAUTH2 struct {
	username string
	token    string
}

func (a *smtpXOAUTH2) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.username, a.token)), nil
}

func (a *smtpXOAUTH2) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return []byte{}, nil
	}
	return nil, nil
}
