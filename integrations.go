package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/web"
)

func mailObject(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func mailToolJSON(value any) (string, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	return string(encoded), err
}

func registerMailTools(mux *http.ServeMux, sqldb *sql.DB, synchronizer *mailSynchronizer, queueSync func(string, int64) error) {
	web.Tool(mux, web.ToolDef{
		Name:        "list_mail_accounts",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "List the user's connected mail accounts, IDs, provider, health, and last sync time. Does not expose credentials.",
		Schema:      mailObject(map[string]any{}),
		Handler: func(ctx context.Context, user auth.User, _ json.RawMessage) (string, error) {
			accounts, err := loadAccounts(ctx, sqldb, user.Login)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(accounts))
			for _, account := range accounts {
				item := map[string]any{"id": account.ID, "provider": account.Provider, "email": account.Email,
					"status": account.Status, "status_detail": account.StatusDetail}
				if account.LastSyncAt != nil {
					item["last_sync_at"] = account.LastSyncAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				out = append(out, item)
			}
			return mailToolJSON(out)
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "search_mail",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "Search the user's locally cached mail by sender, recipient, subject, preview, or cached plain-text body. Returns message links and never contacts a provider.",
		Schema: mailObject(map[string]any{
			"query": map[string]any{"type": "string"},
		}, "query"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(args, &input); err != nil || strings.TrimSpace(input.Query) == "" {
				return "", errors.New("query is required")
			}
			results, err := searchMail(ctx, sqldb, user.Login, input.Query)
			if err != nil {
				return "", err
			}
			return mailToolJSON(results)
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "get_mail_message",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "Read one user-owned message, fetching and caching only its safe plain-text MIME part when needed. HTML and attachment bytes are not returned.",
		Schema: mailObject(map[string]any{
			"message_id": map[string]any{"type": "integer"},
		}, "message_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				MessageID int64 `json:"message_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.MessageID == 0 {
				return "", errors.New("message_id is required")
			}
			message, err := loadMessage(ctx, sqldb, user.Login, input.MessageID)
			if err != nil {
				return "", err
			}
			if message.TextBody == "" {
				if err := synchronizer.FetchBody(ctx, user.Login, input.MessageID); err != nil {
					return "", err
				}
				message, err = loadMessage(ctx, sqldb, user.Login, input.MessageID)
				if err != nil {
					return "", err
				}
			}
			attachments := make([]map[string]any, 0, len(message.Attachments))
			for _, item := range message.Attachments {
				attachments = append(attachments, map[string]any{"id": item.ID, "filename": item.Filename,
					"content_type": item.ContentType, "size_bytes": item.SizeBytes})
			}
			return mailToolJSON(map[string]any{"id": message.ID, "from": message.FromAddress,
				"from_name": message.FromName, "to": message.To, "cc": message.Cc,
				"subject": message.Subject, "received_at": message.ReceivedAt,
				"is_read": message.IsRead, "is_starred": message.IsStarred,
				"body": message.TextBody, "attachments": attachments, "url": fmt.Sprintf("/messages/%d", message.ID)})
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "list_mail_drafts",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "List the user's unsent drafts with IDs, recipients, status, update time, and attachment count for review or follow-up editing.",
		Schema:      mailObject(map[string]any{}),
		Handler: func(ctx context.Context, user auth.User, _ json.RawMessage) (string, error) {
			drafts, err := loadDraftSummaries(ctx, sqldb, user.Login)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(drafts))
			for _, draft := range drafts {
				out = append(out, map[string]any{"id": draft.ID, "to": draft.To, "subject": draft.Subject,
					"status": draft.Status, "status_detail": draft.Detail, "updated_at": draft.UpdatedAt,
					"attachment_count": draft.AttachmentCount, "url": fmt.Sprintf("/drafts/%d", draft.ID)})
			}
			return mailToolJSON(out)
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "get_mail_draft",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "Read one user-owned unsent or sent draft for review. Returns recipients, body, status, and attachment metadata but never attachment bytes.",
		Schema: mailObject(map[string]any{
			"draft_id": map[string]any{"type": "integer"},
		}, "draft_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID int64 `json:"draft_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 {
				return "", errors.New("draft_id is required")
			}
			draft, err := loadDraft(ctx, sqldb, user.Login, input.DraftID)
			if err != nil {
				return "", err
			}
			attachments := make([]map[string]any, 0, len(draft.Attachments))
			for _, item := range draft.Attachments {
				attachments = append(attachments, map[string]any{"id": item.ID, "filename": item.Filename,
					"content_type": item.ContentType, "size_bytes": item.SizeBytes})
			}
			return mailToolJSON(map[string]any{"id": draft.ID, "account_id": draft.AccountID,
				"to": draft.To, "cc": draft.Cc, "bcc": draft.Bcc, "subject": draft.Subject,
				"body": draft.Body, "status": draft.Status, "status_detail": draft.Detail,
				"attachments": attachments, "url": fmt.Sprintf("/drafts/%d", draft.ID)})
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "create_draft",
		Description: "Create an unsent email draft. This never sends mail; recipients and content remain editable in Mail.",
		Schema: mailObject(map[string]any{
			"account_id": map[string]any{"type": "integer", "description": "connected sending account ID; use list_mail_accounts"},
			"to":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "recipient email addresses"},
			"cc":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"bcc":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"subject":    map[string]any{"type": "string"},
			"body":       map[string]any{"type": "string"},
		}),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				AccountID int64    `json:"account_id"`
				To        []string `json:"to"`
				Cc        []string `json:"cc"`
				Bcc       []string `json:"bcc"`
				Subject   string   `json:"subject"`
				Body      string   `json:"body"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &input); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
			}
			id, err := saveDraft(ctx, sqldb, user.Login, 0, input.AccountID, strings.Join(input.To, ", "),
				strings.Join(input.Cc, ", "), strings.Join(input.Bcc, ", "), input.Subject, input.Body)
			if err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Created unsent draft #%d. Review it at /drafts/%d before sending.", id, id), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "bulk_update_messages",
		Description: "Apply one action to 1–50 exact user-owned message IDs. Archive and trash change remote mail and must only run after an explicit user request with the matched messages reviewed.",
		Schema: mailObject(map[string]any{
			"message_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": map[string]any{"type": "integer"}},
			"action":      map[string]any{"type": "string", "enum": []string{"mark-read", "mark-unread", "star", "unstar", "archive", "trash"}},
		}, "message_ids", "action"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				MessageIDs []int64 `json:"message_ids"`
				Action     string  `json:"action"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return "", errors.New("message_ids and action are required")
			}
			accountIDs, err := queueMessageActions(ctx, sqldb, user.Login, input.MessageIDs, input.Action)
			if err != nil {
				return "", err
			}
			for _, accountID := range accountIDs {
				if err := queueSync(user.Login, accountID); err != nil {
					return "", err
				}
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Queued %s for %d message(s) across %d account(s).", input.Action, len(input.MessageIDs), len(accountIDs)), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "create_response_draft",
		Description: "Create an unsent reply, reply-all, or forward draft for one user-owned message. It preserves the original account and never sends mail.",
		Schema: mailObject(map[string]any{
			"message_id": map[string]any{"type": "integer"},
			"mode":       map[string]any{"type": "string", "enum": []string{"reply", "reply-all", "forward"}},
		}, "message_id", "mode"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				MessageID int64  `json:"message_id"`
				Mode      string `json:"mode"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.MessageID == 0 || input.Mode == "" {
				return "", errors.New("message_id and mode are required")
			}
			message, err := loadMessage(ctx, sqldb, user.Login, input.MessageID)
			if err != nil {
				return "", err
			}
			if message.TextBody == "" {
				if err := synchronizer.FetchBody(ctx, user.Login, input.MessageID); err != nil {
					return "", err
				}
			}
			draftID, err := createResponseDraft(ctx, sqldb, user.Login, input.MessageID, input.Mode)
			if err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Created unsent %s draft #%d. Review and edit it at /drafts/%d before sending.",
				input.Mode, draftID, draftID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "update_message",
		Description: "Change one message: mark-read, mark-unread, star, unstar, archive, or trash. Archive and trash move remote mail and must only be used on an explicit user request.",
		Schema: mailObject(map[string]any{
			"message_id": map[string]any{"type": "integer"},
			"action":     map[string]any{"type": "string", "enum": []string{"mark-read", "mark-unread", "star", "unstar", "archive", "trash"}},
		}, "message_id", "action"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				MessageID int64  `json:"message_id"`
				Action    string `json:"action"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.MessageID == 0 {
				return "", errors.New("message_id and action are required")
			}
			accountID, err := queueMessageAction(ctx, sqldb, user.Login, input.MessageID, input.Action)
			if err != nil {
				return "", err
			}
			if err := queueSync(user.Login, accountID); err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Queued %s for message #%d.", input.Action, input.MessageID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "update_draft",
		Description: "Update an existing unsent email draft. This never sends mail and cannot alter an already-sent draft.",
		Schema: mailObject(map[string]any{
			"draft_id":   map[string]any{"type": "integer"},
			"account_id": map[string]any{"type": "integer"},
			"to":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"cc":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"bcc":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"subject":    map[string]any{"type": "string"},
			"body":       map[string]any{"type": "string"},
		}, "draft_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID   int64     `json:"draft_id"`
				AccountID *int64    `json:"account_id"`
				To        *[]string `json:"to"`
				Cc        *[]string `json:"cc"`
				Bcc       *[]string `json:"bcc"`
				Subject   *string   `json:"subject"`
				Body      *string   `json:"body"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 {
				return "", errors.New("draft_id is required")
			}
			d, err := loadDraft(ctx, sqldb, user.Login, input.DraftID)
			if err != nil {
				return "", err
			}
			if input.AccountID != nil {
				d.AccountID = *input.AccountID
			}
			if input.To != nil {
				d.To = *input.To
			}
			if input.Cc != nil {
				d.Cc = *input.Cc
			}
			if input.Bcc != nil {
				d.Bcc = *input.Bcc
			}
			if input.Subject != nil {
				d.Subject = *input.Subject
			}
			if input.Body != nil {
				d.Body = *input.Body
			}
			_, err = saveDraft(ctx, sqldb, user.Login, d.ID, d.AccountID, strings.Join(d.To, ", "),
				strings.Join(d.Cc, ", "), strings.Join(d.Bcc, ", "), d.Subject, d.Body)
			if err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Updated unsent draft #%d. Review it at /drafts/%d before sending.", d.ID, d.ID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "add_draft_text_attachment",
		Description: "Attach generated text to an unsent draft as a local file. This does not send the draft.",
		Schema: mailObject(map[string]any{
			"draft_id": map[string]any{"type": "integer"},
			"filename": map[string]any{"type": "string"},
			"content":  map[string]any{"type": "string"},
		}, "draft_id", "filename", "content"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID  int64  `json:"draft_id"`
				Filename string `json:"filename"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 || input.Filename == "" {
				return "", errors.New("draft_id, filename, and content are required")
			}
			if err := addDraftAttachment(ctx, sqldb, user.Login, input.DraftID, input.Filename,
				"text/plain; charset=utf-8", strings.NewReader(input.Content)); err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Attached %s to draft #%d.", input.Filename, input.DraftID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "remove_draft_attachment",
		Description: "Remove one attachment from an unsent draft. This is destructive and must only be used on an explicit user request.",
		Schema: mailObject(map[string]any{
			"draft_id":      map[string]any{"type": "integer"},
			"attachment_id": map[string]any{"type": "integer"},
		}, "draft_id", "attachment_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID      int64 `json:"draft_id"`
				AttachmentID int64 `json:"attachment_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 || input.AttachmentID == 0 {
				return "", errors.New("draft_id and attachment_id are required")
			}
			if err := removeDraftAttachment(ctx, sqldb, user.Login, input.DraftID, input.AttachmentID); err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Removed attachment #%d from draft #%d.", input.AttachmentID, input.DraftID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "delete_draft",
		Description: "Permanently delete one unsent draft and its staged attachments. This is destructive and must only be used on an explicit user request.",
		Schema: mailObject(map[string]any{
			"draft_id": map[string]any{"type": "integer"},
		}, "draft_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID int64 `json:"draft_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 {
				return "", errors.New("draft_id is required")
			}
			if err := deleteDraft(ctx, sqldb, user.Login, input.DraftID); err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Deleted unsent draft #%d and its staged attachments.", input.DraftID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "send_draft",
		Description: "Send one existing draft through its selected account. This transmits email externally and must only be used on an explicit user request after the user has reviewed the draft.",
		Schema: mailObject(map[string]any{
			"draft_id": map[string]any{"type": "integer", "description": "the reviewed draft ID to send"},
		}, "draft_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				DraftID int64 `json:"draft_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.DraftID == 0 {
				return "", errors.New("draft_id is required")
			}
			if err := synchronizer.SendDraft(ctx, user.Login, input.DraftID); err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Sent draft #%d.", input.DraftID), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "sync_accounts",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "Queue mail synchronization for all connected Gmail and iCloud accounts.",
		Schema:      mailObject(map[string]any{}),
		Handler: func(ctx context.Context, user auth.User, _ json.RawMessage) (string, error) {
			rows, err := sqldb.QueryContext(ctx, "SELECT id FROM accounts WHERE login=? AND status!='disabled' ORDER BY id", user.Login)
			if err != nil {
				return "", err
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return "", err
				}
				if err := queueSync(user.Login, id); err != nil {
					return "", err
				}
				count++
			}
			if err := rows.Err(); err != nil {
				return "", err
			}
			return fmt.Sprintf("Queued synchronization for %d account(s).", count), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "check_mail_account",
		Automation:  web.AutomationPolicy{Mode: web.AutomationReadOnly},
		Description: "Verify IMAP and SMTP authentication for one connected mail account without reading messages or sending email.",
		Schema: mailObject(map[string]any{
			"account_id": map[string]any{"type": "integer"},
		}, "account_id"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				AccountID int64 `json:"account_id"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.AccountID == 0 {
				return "", errors.New("account_id is required")
			}
			result, err := synchronizer.CheckAccount(ctx, user.Login, input.AccountID)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s connection passed for %s: IMAP and SMTP authenticated; %d mailboxes visible.",
				strings.ToUpper(result.Provider), result.Email, result.Mailboxes), nil
		},
	})

	web.Tool(mux, web.ToolDef{
		Name:        "disconnect_mail_account",
		Description: "Permanently remove one connected account, its encrypted credentials, cached mail, and queued operations from Bespoke. Unsent drafts remain without a sending account. This is destructive and must only run on an explicit user request; provider-side OAuth or app-password access must be revoked separately.",
		Schema: mailObject(map[string]any{
			"account_id": map[string]any{"type": "integer"},
			"confirm":    map[string]any{"type": "boolean", "description": "must be true after explicit user confirmation"},
		}, "account_id", "confirm"),
		Handler: func(ctx context.Context, user auth.User, args json.RawMessage) (string, error) {
			var input struct {
				AccountID int64 `json:"account_id"`
				Confirm   bool  `json:"confirm"`
			}
			if err := json.Unmarshal(args, &input); err != nil || input.AccountID == 0 {
				return "", errors.New("account_id and explicit confirmation are required")
			}
			removed, err := disconnectMailAccount(ctx, sqldb, user.Login, input.AccountID, input.Confirm)
			if err != nil {
				return "", err
			}
			web.Changed(user.Login)
			return fmt.Sprintf("Disconnected %s %s from Bespoke. Provider-side access was not revoked.",
				strings.ToUpper(removed.Provider), removed.Email), nil
		},
	})
}

func searchMail(ctx context.Context, sqldb *sql.DB, login, query string) ([]web.SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	rows, err := sqldb.QueryContext(ctx, `SELECT DISTINCT m.id, m.subject,
		COALESCE((SELECT address FROM addresses WHERE message_id=m.id AND kind='from' ORDER BY position LIMIT 1), ''),
		m.preview, m.received_at
		FROM messages m JOIN accounts a ON a.id=m.account_id
		LEFT JOIN addresses addr ON addr.message_id=m.id
		WHERE a.login=? AND (m.subject LIKE '%'||?||'%' ESCAPE '\'
			OR m.preview LIKE '%'||?||'%' ESCAPE '\'
			OR m.text_body LIKE '%'||?||'%' ESCAPE '\'
			OR addr.name LIKE '%'||?||'%' ESCAPE '\'
			OR addr.address LIKE '%'||?||'%' ESCAPE '\')
		ORDER BY m.received_at DESC LIMIT 20`, login, escaped, escaped, escaped, escaped, escaped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []web.SearchResult
	for rows.Next() {
		var id int64
		var subject, sender, preview, received string
		if err := rows.Scan(&id, &subject, &sender, &preview, &received); err != nil {
			return nil, err
		}
		if subject == "" {
			subject = "(No subject)"
		}
		snippet := sender
		if preview != "" {
			snippet += " — " + preview
		}
		out = append(out, web.SearchResult{Title: subject, Snippet: snippet,
			URL: fmt.Sprintf("/messages/%d", id), Timestamp: received})
	}
	return out, rows.Err()
}
