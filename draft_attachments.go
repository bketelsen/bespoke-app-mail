package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

const maxDraftAttachmentBytes = 25 << 20

type draftAttachment struct {
	ID          int64
	Filename    string
	ContentType string
	SizeBytes   int64
	CacheKey    string
}

func mailAttachmentDir() string {
	dir := os.Getenv("BESPOKE_DATA")
	if dir == "" {
		root := os.Getenv("BESPOKE_ROOT")
		if root == "" {
			root = "."
		}
		dir = filepath.Join(root, "data")
	}
	return filepath.Join(dir, "mail-attachments")
}

func draftAttachmentPath(cacheKey string) (string, error) {
	decoded, err := hex.DecodeString(cacheKey)
	if err != nil || len(decoded) != 24 {
		return "", errors.New("invalid attachment cache key")
	}
	return filepath.Join(mailAttachmentDir(), cacheKey), nil
}

func addDraftAttachment(ctx context.Context, sqldb *sql.DB, login string, draftID int64, filename, contentType string, src io.Reader) error {
	var currentBytes int64
	var drafts int
	if err := sqldb.QueryRowContext(ctx, `SELECT COALESCE(SUM(da.size_bytes), 0), COUNT(DISTINCT d.id)
		FROM drafts d LEFT JOIN draft_attachments da ON da.draft_id=d.id
		WHERE d.id=? AND d.login=? AND d.status IN ('draft','error')`, draftID, login).Scan(&currentBytes, &drafts); err != nil || drafts != 1 {
		return errors.New("editable draft not found")
	}
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || strings.ContainsAny(filename, "\r\n") {
		filename = "attachment"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	} else if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = parsed
	} else {
		contentType = "application/octet-stream"
	}
	if err := os.MkdirAll(mailAttachmentDir(), 0o700); err != nil {
		return err
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	cacheKey := hex.EncodeToString(random)
	path, err := draftAttachmentPath(cacheKey)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(src, maxDraftAttachmentBytes-currentBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || currentBytes+written > maxDraftAttachmentBytes {
		_ = os.Remove(path)
		if currentBytes+written > maxDraftAttachmentBytes {
			return fmt.Errorf("draft attachments may total at most %d MiB", maxDraftAttachmentBytes>>20)
		}
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	if written == 0 {
		_ = os.Remove(path)
		return errors.New("attachment is empty")
	}
	result, err := sqldb.ExecContext(ctx, `INSERT INTO draft_attachments
		(draft_id, filename, content_type, size_bytes, cache_key)
		SELECT id, ?, ?, ?, ? FROM drafts WHERE id=? AND login=? AND status IN ('draft','error')`,
		filename, contentType, written, cacheKey, draftID, login)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		_ = os.Remove(path)
		return errors.New("editable draft not found")
	}
	return nil
}

func removeDraftAttachment(ctx context.Context, sqldb *sql.DB, login string, draftID, attachmentID int64) error {
	var cacheKey string
	err := sqldb.QueryRowContext(ctx, `DELETE FROM draft_attachments WHERE id=? AND draft_id IN
		(SELECT id FROM drafts WHERE id=? AND login=? AND status IN ('draft','error')) RETURNING cache_key`,
		attachmentID, draftID, login).Scan(&cacheKey)
	if err != nil {
		return err
	}
	path, err := draftAttachmentPath(cacheKey)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func deleteDraft(ctx context.Context, sqldb *sql.DB, login string, draftID int64) error {
	attachments, err := loadDraftAttachmentData(ctx, sqldb, login, draftID)
	if err != nil {
		return err
	}
	result, err := sqldb.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND login=?
		AND status IN ('draft','error')`, draftID, login)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("editable draft not found")
	}
	for _, attachment := range attachments {
		path, pathErr := draftAttachmentPath(attachment.CacheKey)
		if pathErr != nil {
			continue
		}
		_ = os.Remove(path)
	}
	return nil
}

func loadDraftAttachmentData(ctx context.Context, sqldb *sql.DB, login string, draftID int64) ([]draftAttachment, error) {
	rows, err := sqldb.QueryContext(ctx, `SELECT da.id, da.filename, da.content_type, da.size_bytes, da.cache_key
		FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id
		WHERE da.draft_id=? AND d.login=? ORDER BY da.id`, draftID, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attachments []draftAttachment
	for rows.Next() {
		var item draftAttachment
		if err := rows.Scan(&item.ID, &item.Filename, &item.ContentType, &item.SizeBytes, &item.CacheKey); err != nil {
			return nil, err
		}
		attachments = append(attachments, item)
	}
	return attachments, rows.Err()
}
