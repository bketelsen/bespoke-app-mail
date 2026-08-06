package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bketelsen/bespoke/pkg/events"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/microcosm-cc/bluemonday"
)

const (
	initialMessageLimit = 100
	maxSyncedMailboxes  = 20
)

type syncAccount struct {
	ID         int64
	Login      string
	Provider   string
	Email      string
	Ciphertext []byte
	Nonce      []byte
}

const maxCachedTextBody = 2 << 20
const maxCachedHTMLBody = 4 << 20
const maxAttachmentDownload = 25 << 20

type attachmentDownload struct {
	Filename    string
	ContentType string
	Data        []byte
}

// FetchBody retrieves and caches the preferred text/plain and text/html MIME
// parts. It never requests attachment bytes; HTML is sanitized when served.
func (s *mailSynchronizer) FetchBody(ctx context.Context, login string, messageID int64) error {
	var a syncAccount
	var mailboxName string
	var remoteUID uint32
	err := s.db.QueryRowContext(ctx, `SELECT a.id, a.login, a.provider, a.email,
		a.credential_ciphertext, a.credential_nonce, mb.remote_name, mm.remote_uid
		FROM messages m JOIN accounts a ON a.id=m.account_id
		JOIN message_mailboxes mm ON mm.message_id=m.id
		JOIN mailboxes mb ON mb.id=mm.mailbox_id
		WHERE m.id=? AND a.login=? LIMIT 1`, messageID, login).Scan(
		&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce, &mailboxName, &remoteUID)
	if err != nil {
		return err
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
	client, err := openIMAP(ctx, a, credential)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Select(mailboxName, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return fmt.Errorf("select message mailbox: %w", err)
	}
	uidSet := imap.UIDSetNum(imap.UID(remoteUID))
	structureMessages, err := client.Fetch(uidSet, &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil || len(structureMessages) != 1 || structureMessages[0].BodyStructure == nil {
		return errors.New("message body structure is unavailable")
	}
	if err := s.cacheAttachmentMetadata(ctx, messageID, structureMessages[0].BodyStructure); err != nil {
		return err
	}
	textPath, textSize, hasText := preferredTextPart(structureMessages[0].BodyStructure)
	htmlPath, htmlSize, hasHTML := preferredHTMLPart(structureMessages[0].BodyStructure)
	if !hasText && !hasHTML {
		_, err := s.db.ExecContext(ctx, `UPDATE messages SET text_body='', html_body='', body_fetched=1,
			preview='Message has no readable body', updated_at=datetime('now') WHERE id=?`, messageID)
		return err
	}
	if textSize > maxCachedTextBody {
		return fmt.Errorf("plain-text message body exceeds %d bytes", maxCachedTextBody)
	}
	if htmlSize > maxCachedHTMLBody {
		return fmt.Errorf("HTML message body exceeds %d bytes", maxCachedHTMLBody)
	}
	text, err := fetchMessagePart(client, uidSet, textPath, hasText, maxCachedTextBody)
	if err != nil {
		return fmt.Errorf("fetch plain-text message body: %w", err)
	}
	htmlBody, err := fetchMessagePart(client, uidSet, htmlPath, hasHTML, maxCachedHTMLBody)
	if err != nil {
		return fmt.Errorf("fetch HTML message body: %w", err)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	preview := messagePreview(text)
	if preview == "" && htmlBody != "" {
		preview = messagePreview(bluemonday.StrictPolicy().Sanitize(htmlBody))
	}
	_, err = s.db.ExecContext(ctx, `UPDATE messages SET text_body=?, html_body=?, body_fetched=1,
		preview=?, updated_at=datetime('now') WHERE id=?`, text, htmlBody, preview, messageID)
	return err
}

func fetchMessagePart(client *imapclient.Client, uidSet imap.UIDSet, path []int, present bool, limit int64) (string, error) {
	if !present {
		return "", nil
	}
	mimeSection := &imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME, Peek: true}
	bodySection := &imap.FetchItemBodySection{Part: path, Peek: true}
	fetched, err := client.Fetch(uidSet, &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{mimeSection, bodySection}}).Collect()
	if err != nil || len(fetched) != 1 {
		return "", errors.New("message part is unavailable")
	}
	raw := append(bytes.Clone(fetched[0].FindBodySection(mimeSection)), fetched[0].FindBodySection(bodySection)...)
	entity, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode MIME part: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(entity.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > limit {
		return "", fmt.Errorf("decoded message part exceeds %d bytes", limit)
	}
	return string(body), nil
}

func (s *mailSynchronizer) FetchAttachment(ctx context.Context, login string, attachmentID int64) (attachmentDownload, error) {
	var a syncAccount
	var mailboxName, partPath, filename, contentType string
	var remoteUID uint32
	err := s.db.QueryRowContext(ctx, `SELECT a.id, a.login, a.provider, a.email,
		a.credential_ciphertext, a.credential_nonce, mb.remote_name, mm.remote_uid,
		att.part_path, att.filename, att.content_type
		FROM attachments att JOIN messages m ON m.id=att.message_id
		JOIN accounts a ON a.id=m.account_id JOIN message_mailboxes mm ON mm.message_id=m.id
		JOIN mailboxes mb ON mb.id=mm.mailbox_id
		WHERE att.id=? AND a.login=? LIMIT 1`, attachmentID, login).Scan(
		&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce, &mailboxName, &remoteUID,
		&partPath, &filename, &contentType)
	if err != nil {
		return attachmentDownload{}, err
	}
	credential, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return attachmentDownload{}, err
	}
	if a.Provider == "gmail" {
		credential, err = s.freshGoogleCredential(ctx, a, credential)
		if err != nil {
			return attachmentDownload{}, err
		}
	}
	client, err := openIMAP(ctx, a, credential)
	if err != nil {
		return attachmentDownload{}, err
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Select(mailboxName, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return attachmentDownload{}, err
	}
	pathParts := strings.Split(partPath, ".")
	path := make([]int, len(pathParts))
	for i, part := range pathParts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 1 {
			return attachmentDownload{}, errors.New("invalid attachment part")
		}
		path[i] = value
	}
	mimeSection := &imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME, Peek: true}
	bodySection := &imap.FetchItemBodySection{Part: path, Peek: true}
	fetched, err := client.Fetch(imap.UIDSetNum(imap.UID(remoteUID)), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{mimeSection, bodySection},
	}).Collect()
	if err != nil || len(fetched) != 1 {
		return attachmentDownload{}, errors.New("fetch attachment")
	}
	raw := append(bytes.Clone(fetched[0].FindBodySection(mimeSection)), fetched[0].FindBodySection(bodySection)...)
	entity, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		return attachmentDownload{}, fmt.Errorf("decode attachment: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(entity.Body, maxAttachmentDownload+1))
	if err != nil {
		return attachmentDownload{}, err
	}
	if len(data) > maxAttachmentDownload {
		return attachmentDownload{}, fmt.Errorf("attachment exceeds %d bytes", maxAttachmentDownload)
	}
	return attachmentDownload{Filename: filename, ContentType: contentType, Data: data}, nil
}

func (s *mailSynchronizer) cacheAttachmentMetadata(ctx context.Context, messageID int64, structure imap.BodyStructure) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM attachments WHERE message_id=?", messageID); err != nil {
		return err
	}
	count := 0
	var walkErr error
	structure.Walk(func(path []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		filename := single.Filename()
		disposition := single.Disposition()
		isAttachment := filename != "" || (disposition != nil && strings.EqualFold(disposition.Value, "attachment"))
		if !isAttachment {
			return true
		}
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d", count+1)
		}
		parts := make([]string, len(path))
		for i, value := range path {
			parts[i] = fmt.Sprint(value)
		}
		_, walkErr = tx.ExecContext(ctx, `INSERT INTO attachments
			(message_id, part_path, filename, content_type, content_id, size_bytes)
			VALUES (?, ?, ?, ?, ?, ?)`, messageID, strings.Join(parts, "."), filename,
			single.MediaType(), single.ID, single.Size)
		if walkErr == nil {
			count++
		}
		return walkErr == nil
	})
	if walkErr != nil {
		return walkErr
	}
	if _, err := tx.ExecContext(ctx, "UPDATE messages SET has_attachments=? WHERE id=?", count > 0, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

func preferredTextPart(structure imap.BodyStructure) ([]int, uint32, bool) {
	return preferredInlinePart(structure, "text/plain")
}

func preferredHTMLPart(structure imap.BodyStructure) ([]int, uint32, bool) {
	return preferredInlinePart(structure, "text/html")
}

func preferredInlinePart(structure imap.BodyStructure, mediaType string) ([]int, uint32, bool) {
	var path []int
	var size uint32
	structure.Walk(func(candidate []int, part imap.BodyStructure) bool {
		if path != nil {
			return false
		}
		if part.MediaType() != mediaType {
			return true
		}
		if disposition := part.Disposition(); disposition != nil && strings.EqualFold(disposition.Value, "attachment") {
			return false
		}
		path = slices.Clone(candidate)
		if single, ok := part.(*imap.BodyStructureSinglePart); ok {
			size = single.Size
		}
		return false
	})
	return path, size, path != nil
}

func messagePreview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 180 {
		return string(runes[:180]) + "…"
	}
	return text
}

type mailSynchronizer struct {
	db     *sql.DB
	vault  *credentialVault
	google *googleOAuthConfig
}

type accountCheck struct {
	Provider  string
	Email     string
	Mailboxes int
}

// CheckAccount verifies both receiving and sending authentication but never
// selects message content or submits an SMTP envelope.
func (s *mailSynchronizer) CheckAccount(ctx context.Context, login string, accountID int64) (accountCheck, error) {
	if s.vault == nil {
		return accountCheck{}, errors.New("credential encryption is unavailable")
	}
	var a syncAccount
	err := s.db.QueryRowContext(ctx, `SELECT id, login, provider, email,
		credential_ciphertext, credential_nonce FROM accounts WHERE id=? AND login=? AND status!='disabled'`, accountID, login).
		Scan(&a.ID, &a.Login, &a.Provider, &a.Email, &a.Ciphertext, &a.Nonce)
	if err != nil {
		return accountCheck{}, err
	}
	credential, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return accountCheck{}, err
	}
	if a.Provider == "gmail" {
		credential, err = s.freshGoogleCredential(ctx, a, credential)
		if err != nil {
			return accountCheck{}, err
		}
	}
	imapClient, err := openIMAP(ctx, a, credential)
	if err != nil {
		return accountCheck{}, err
	}
	mailboxes, err := imapClient.List("", "*", nil).Collect()
	_ = imapClient.Close()
	if err != nil {
		return accountCheck{}, fmt.Errorf("list mailboxes: %w", err)
	}
	smtpClient, err := openSMTP(ctx, a, credential)
	if err != nil {
		return accountCheck{}, err
	}
	_ = smtpClient.Quit()
	return accountCheck{Provider: a.Provider, Email: a.Email, Mailboxes: len(mailboxes)}, nil
}

func (s *mailSynchronizer) SyncAccount(ctx context.Context, login string, accountID int64) error {
	if s.vault == nil {
		return errors.New("credential encryption is unavailable")
	}
	var a syncAccount
	var prevStatus string
	err := s.db.QueryRowContext(ctx, `SELECT id, login, provider, email, status,
		credential_ciphertext, credential_nonce FROM accounts WHERE id=? AND login=?`, accountID, login).
		Scan(&a.ID, &a.Login, &a.Provider, &a.Email, &prevStatus, &a.Ciphertext, &a.Nonce)
	if err != nil {
		return err
	}
	credential, err := s.vault.Open(a.Ciphertext, a.Nonce)
	if err != nil {
		return err
	}
	if a.Provider == "gmail" {
		credential, err = s.freshGoogleCredential(ctx, a, credential)
		if err != nil {
			s.markSyncError(ctx, a, prevStatus, err)
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET status='syncing',
		status_detail='Connecting', updated_at=datetime('now') WHERE id=?`, a.ID); err != nil {
		return err
	}

	err = s.syncIMAP(ctx, a, credential)
	if err != nil {
		s.markSyncError(ctx, a, prevStatus, err)
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounts SET status='healthy', status_detail='',
		last_sync_at=datetime('now'), updated_at=datetime('now') WHERE id=?`, a.ID)
	return err
}

// markSyncError records the failure and publishes mail.account_sync_failed
// only on a non-error→error transition, so the 5-minute poller does not
// notify again while the account stays broken.
func (s *mailSynchronizer) markSyncError(ctx context.Context, a syncAccount, prevStatus string, syncErr error) {
	ctx = context.WithoutCancel(ctx)
	detail := truncateError(syncErr)
	_, _ = s.db.ExecContext(ctx, `UPDATE accounts SET status='error',
		status_detail=?, updated_at=datetime('now') WHERE id=?`, detail, a.ID)
	if prevStatus == "error" {
		return
	}
	publishMailEvent(ctx, a.Login, events.Event{
		Type:      "mail.account_sync_failed",
		SubjectID: fmt.Sprint(a.ID),
		Data: map[string]any{"account_id": a.ID, "email": a.Email,
			"provider": a.Provider, "detail": detail},
		Notification: &events.Notification{
			Title:    truncateBytes("Mail sync failing for "+a.Email, maxNotificationTitleBytes),
			Body:     truncateBytes(detail, maxNotificationBodyBytes),
			AppSlug:  "mail",
			Path:     "/settings/accounts",
			GroupKey: "mail:sync:" + a.Email,
		},
	})
}

func (s *mailSynchronizer) syncIMAP(ctx context.Context, a syncAccount, credential accountCredential) error {
	client, err := openIMAP(ctx, a, credential)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	listed, err := client.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		listed, err = client.List("", "*", nil).Collect()
	}
	if err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}

	inboxName := ""
	for position, item := range listed {
		selectable, err := s.storeMailboxListing(ctx, a, item, position)
		if err != nil {
			return err
		}
		if !selectable {
			continue
		}
		role := mailboxRole(a.Provider, item.Mailbox, item.Attrs)
		if role == "inbox" {
			inboxName = item.Mailbox
		}
	}
	if inboxName == "" {
		inboxName = "INBOX"
		if _, err := s.db.ExecContext(ctx, `INSERT INTO mailboxes
			(account_id, remote_name, display_name, role, position) VALUES (?, ?, 'Inbox', 'inbox', 0)
			ON CONFLICT(account_id, remote_name) DO UPDATE SET role='inbox'`, a.ID, inboxName); err != nil {
			return err
		}
	}
	if err := s.applyPending(ctx, client, a); err != nil {
		return err
	}

	// Keep a useful, bounded local view of the account instead of making every
	// folder click wait on the provider. Core folders are ordered first and the
	// cap prevents pathological accounts with hundreds of labels from turning a
	// scheduled refresh into an unbounded job.
	rows, err := s.db.QueryContext(ctx, `SELECT remote_name FROM mailboxes WHERE account_id=?
		ORDER BY CASE role WHEN 'inbox' THEN 0 WHEN 'sent' THEN 1 WHEN 'drafts' THEN 2
		WHEN 'archive' THEN 3 WHEN 'trash' THEN 4 WHEN 'spam' THEN 5 ELSE 6 END,
		position, id LIMIT ?`, a.ID, maxSyncedMailboxes)
	if err != nil {
		return err
	}
	var mailboxNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		mailboxNames = append(mailboxNames, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(mailboxNames) == 0 {
		mailboxNames = []string{inboxName}
	}
	for _, name := range mailboxNames {
		if err := s.syncMailbox(ctx, client, a, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *mailSynchronizer) storeMailboxListing(ctx context.Context, a syncAccount, item *imap.ListData, position int) (bool, error) {
	if !isSelectableMailbox(a.Provider, item) {
		_, err := s.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE account_id=? AND remote_name=?`, a.ID, item.Mailbox)
		return false, err
	}
	role := mailboxRole(a.Provider, item.Mailbox, item.Attrs)
	_, err := s.db.ExecContext(ctx, `INSERT INTO mailboxes
		(account_id, remote_name, display_name, role, position) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(account_id, remote_name) DO UPDATE SET
		display_name=excluded.display_name, role=excluded.role, position=excluded.position`,
		a.ID, item.Mailbox, mailboxDisplayName(item.Mailbox), role, position)
	return true, err
}

func isSelectableMailbox(provider string, item *imap.ListData) bool {
	if mailboxHasAttr(item.Attrs, imap.MailboxAttrNoSelect) {
		return false
	}
	// Gmail exposes [Gmail] as the namespace containing its special-use
	// mailboxes. Some LIST responses omit \Noselect even though SELECT always
	// rejects this exact container with NONEXISTENT.
	return provider != "gmail" || !strings.EqualFold(item.Mailbox, "[Gmail]")
}

func openIMAP(ctx context.Context, a syncAccount, credential accountCredential) (*imapclient.Client, error) {
	server := ""
	switch a.Provider {
	case "icloud":
		server = "imap.mail.me.com:993"
	case "gmail":
		server = "imap.gmail.com:993"
	default:
		return nil, fmt.Errorf("unsupported provider %q", a.Provider)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}
	client, err := imapclient.DialTLS(server, &imapclient.Options{Dialer: dialer})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", a.Provider, err)
	}
	switch a.Provider {
	case "icloud":
		username := strings.SplitN(a.Email, "@", 2)[0]
		if err := client.Login(username, credential.AppPassword).Wait(); err != nil {
			// Apple documents the local part as usual and the full address as a
			// fallback, so try the latter on a fresh connection.
			_ = client.Close()
			client, err = imapclient.DialTLS(server, &imapclient.Options{Dialer: dialer})
			if err != nil {
				return nil, fmt.Errorf("reconnect to iCloud: %w", err)
			}
			if err := client.Login(a.Email, credential.AppPassword).Wait(); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("authenticate iCloud account: %w", err)
			}
		}
	case "gmail":
		if credential.AccessToken == "" {
			_ = client.Close()
			return nil, errors.New("google authorization needs to be renewed")
		}
		if err := client.Authenticate(&xoauth2Client{username: a.Email, token: credential.AccessToken}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("authenticate Gmail account: %w", err)
		}
	}
	return client, nil
}

func (s *mailSynchronizer) freshGoogleCredential(ctx context.Context, a syncAccount, credential accountCredential) (accountCredential, error) {
	expires, ok := parseDBTime(credential.TokenExpiry)
	if ok && time.Until(expires) > 2*time.Minute {
		return credential, nil
	}
	token, err := s.google.refresh(ctx, credential.RefreshToken)
	if err != nil {
		return accountCredential{}, err
	}
	credential.AccessToken = token.AccessToken
	credential.TokenExpiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	if token.RefreshToken != "" {
		credential.RefreshToken = token.RefreshToken
	}
	ciphertext, nonce, err := s.vault.Seal(credential)
	if err != nil {
		return accountCredential{}, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounts SET credential_ciphertext=?, credential_nonce=?,
		updated_at=datetime('now') WHERE id=?`, ciphertext, nonce, a.ID)
	return credential, err
}

func (s *mailSynchronizer) syncMailbox(ctx context.Context, client *imapclient.Client, a syncAccount, remoteName string) error {
	selected, err := client.Select(remoteName, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("select mailbox %q: %w", remoteName, err)
	}
	var mailboxID int64
	var oldValidity, highestUID uint32
	var role string
	err = s.db.QueryRowContext(ctx, `SELECT id, uid_validity, highest_uid, role FROM mailboxes
		WHERE account_id=? AND remote_name=?`, a.ID, remoteName).Scan(&mailboxID, &oldValidity, &highestUID, &role)
	if err != nil {
		return err
	}
	if oldValidity != 0 && oldValidity != selected.UIDValidity {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id IN
			(SELECT message_id FROM message_mailboxes WHERE mailbox_id=?)`, mailboxID); err != nil {
			return err
		}
		highestUID = 0
	}
	// A zero high-water mark means a first sync or UIDVALIDITY rebuild: every
	// message is "new" to the cache, so backfill publishes no events.
	backfill := highestUID == 0

	if selected.NumMessages == 0 {
		_, err = s.db.ExecContext(ctx, `UPDATE mailboxes SET uid_validity=?, highest_uid=0,
			total_count=0, unread_count=0 WHERE id=?`, selected.UIDValidity, mailboxID)
		return err
	}

	start := uint32(1)
	if selected.NumMessages > initialMessageLimit {
		start = selected.NumMessages - initialMessageLimit + 1
	}
	seqs := imap.SeqSet{}
	seqs.AddRange(start, selected.NumMessages)

	messages, err := client.Fetch(seqs, &imap.FetchOptions{
		UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
	}).Collect()
	if err != nil {
		return fmt.Errorf("fetch mailbox %q: %w", remoteName, err)
	}
	maxUID := uint32(0)
	notified := 0
	remoteUIDs := make(map[uint32]struct{}, len(messages))
	for _, fetched := range messages {
		if fetched.Envelope == nil || fetched.UID == 0 {
			continue
		}
		if uint32(fetched.UID) > maxUID {
			maxUID = uint32(fetched.UID)
		}
		remoteUIDs[uint32(fetched.UID)] = struct{}{}
		messageID, inserted, err := s.storeEnvelope(ctx, a, mailboxID, fetched)
		if err != nil {
			return err
		}
		if inserted && !backfill && role == "inbox" && !slices.Contains(fetched.Flags, imap.FlagSeen) {
			s.publishMailReceived(ctx, a, messageID, fetched.Envelope, notified < maxNewMailNotifications)
			notified++
		}
	}
	if err := s.reconcileMailboxWindow(ctx, mailboxID, remoteUIDs); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE mailboxes SET uid_validity=?, highest_uid=?,
		total_count=?, unread_count=(SELECT COUNT(*) FROM messages m JOIN message_mailboxes mm
		ON mm.message_id=m.id WHERE mm.mailbox_id=? AND m.is_read=0) WHERE id=?`,
		selected.UIDValidity, maxUID, selected.NumMessages, mailboxID, mailboxID)
	return err
}

func (s *mailSynchronizer) reconcileMailboxWindow(ctx context.Context, mailboxID int64, remoteUIDs map[uint32]struct{}) error {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, remote_uid FROM message_mailboxes WHERE mailbox_id=?`, mailboxID)
	if err != nil {
		return err
	}
	type cachedMessage struct {
		messageID int64
		uid       uint32
	}
	var stale []cachedMessage
	for rows.Next() {
		var cached cachedMessage
		if err := rows.Scan(&cached.messageID, &cached.uid); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := remoteUIDs[cached.uid]; !ok {
			stale = append(stale, cached)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, cached := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_mailboxes WHERE mailbox_id=? AND remote_uid=?`, mailboxID, cached.uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=?
			AND NOT EXISTS (SELECT 1 FROM message_mailboxes WHERE message_id=?)`, cached.messageID, cached.messageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// storeEnvelope upserts one fetched message and reports whether it created a
// new local row. Sync is single-goroutine, so the pre-select cannot race the
// upsert below it.
func (s *mailSynchronizer) storeEnvelope(ctx context.Context, a syncAccount, mailboxID int64, fetched *imapclient.FetchMessageBuffer) (int64, bool, error) {
	envelope := fetched.Envelope
	received := fetched.InternalDate
	if received.IsZero() {
		received = envelope.Date
	}
	if received.IsZero() {
		received = time.Now().UTC()
	}
	remoteKey := fmt.Sprintf("%d:%d", mailboxID, fetched.UID)
	var existingID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id=? AND remote_key=?`,
		a.ID, remoteKey).Scan(&existingID)
	inserted := errors.Is(err, sql.ErrNoRows)
	if err != nil && !inserted {
		return 0, false, err
	}
	var messageID int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO messages
		(account_id, remote_key, message_id, in_reply_to, subject, sent_at, received_at,
		is_read, is_starred, size_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, remote_key) DO UPDATE SET subject=excluded.subject,
		message_id=excluded.message_id, in_reply_to=excluded.in_reply_to,
		sent_at=excluded.sent_at, received_at=excluded.received_at,
		is_read=excluded.is_read, is_starred=excluded.is_starred,
		size_bytes=excluded.size_bytes, updated_at=datetime('now') RETURNING id`,
		a.ID, remoteKey, envelope.MessageID, strings.Join(envelope.InReplyTo, " "), envelope.Subject,
		nullTime(envelope.Date), received.UTC().Format(time.RFC3339Nano),
		slices.Contains(fetched.Flags, imap.FlagSeen), slices.Contains(fetched.Flags, imap.FlagFlagged), fetched.RFC822Size).Scan(&messageID)
	if err != nil {
		return 0, false, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO message_mailboxes (message_id, mailbox_id, remote_uid)
		VALUES (?, ?, ?) ON CONFLICT(mailbox_id, remote_uid) DO UPDATE SET message_id=excluded.message_id`,
		messageID, mailboxID, fetched.UID); err != nil {
		return 0, false, err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM addresses WHERE message_id=?", messageID); err != nil {
		return 0, false, err
	}
	for kind, addresses := range map[string][]imap.Address{
		"from": envelope.From, "to": envelope.To, "cc": envelope.Cc, "bcc": envelope.Bcc, "reply-to": envelope.ReplyTo,
	} {
		for position, address := range addresses {
			if address.Addr() == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO addresses
				(message_id, kind, position, name, address) VALUES (?, ?, ?, ?, ?)`,
				messageID, kind, position, address.Name, address.Addr()); err != nil {
				return 0, false, err
			}
		}
	}
	return messageID, inserted, nil
}

// publishMailReceived reports one genuinely new, unread inbox message. Beyond
// the per-mailbox notification cap the event still publishes without a
// notification. The body structure has not been fetched yet, so the preview
// and attachment flag are limited to what the envelope carries.
func (s *mailSynchronizer) publishMailReceived(ctx context.Context, a syncAccount, messageID int64, envelope *imap.Envelope, notify bool) {
	fromName, fromAddr := "", ""
	if len(envelope.From) > 0 {
		fromName, fromAddr = envelope.From[0].Name, envelope.From[0].Addr()
	}
	sender := fromName
	if sender == "" {
		sender = fromAddr
	}
	event := events.Event{
		Type:      "mail.received",
		SubjectID: fmt.Sprint(messageID),
		Data: map[string]any{"message_id": messageID, "account": a.Email, "from": fromAddr,
			"from_name": fromName, "subject": envelope.Subject, "has_attachments": false},
	}
	if notify {
		title := "New message"
		if sender != "" {
			title = "New message from " + sender
		}
		event.Notification = &events.Notification{
			Title:    truncateBytes(title, maxNotificationTitleBytes),
			Body:     truncateBytes(envelope.Subject, maxNotificationBodyBytes),
			AppSlug:  "mail",
			Path:     fmt.Sprintf("/messages/%d", messageID),
			GroupKey: "mail:" + a.Email,
		}
	}
	publishMailEvent(ctx, a.Login, event)
}

func mailboxRole(provider, name string, attrs []imap.MailboxAttr) string {
	roles := []struct {
		attr imap.MailboxAttr
		role string
	}{{imap.MailboxAttrSent, "sent"}, {imap.MailboxAttrDrafts, "drafts"},
		{imap.MailboxAttrAll, "archive"}, {imap.MailboxAttrArchive, "archive"},
		{imap.MailboxAttrTrash, "trash"}, {imap.MailboxAttrJunk, "spam"}}
	for _, candidate := range roles {
		if mailboxHasAttr(attrs, candidate.attr) {
			return candidate.role
		}
	}
	if strings.EqualFold(name, "INBOX") {
		return "inbox"
	}
	// iCloud's IMAP LIST response does not consistently advertise SPECIAL-USE
	// attributes. Its default folders have stable names, so recognize those
	// names only for iCloud accounts rather than guessing roles for user folders
	// on every provider.
	if provider == "icloud" {
		switch {
		case strings.EqualFold(name, "Archive"):
			return "archive"
		case strings.EqualFold(name, "Sent Messages"):
			return "sent"
		case strings.EqualFold(name, "Drafts"):
			return "drafts"
		case strings.EqualFold(name, "Deleted Messages"):
			return "trash"
		case strings.EqualFold(name, "Junk"):
			return "spam"
		}
	}
	return "folder"
}

func mailboxHasAttr(attrs []imap.MailboxAttr, wanted imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if strings.EqualFold(string(attr), string(wanted)) {
			return true
		}
	}
	return false
}

func mailboxDisplayName(name string) string {
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func truncateError(err error) string {
	value := strings.TrimSpace(err.Error())
	if len(value) > 300 {
		value = value[:300] + "…"
	}
	return value
}
