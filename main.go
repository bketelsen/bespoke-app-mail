// mail: a private, local-first Gmail and iCloud client. Product contract:
// README.md.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bketelsen/bespoke-app-mail/views"
	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/db"
	"github.com/bketelsen/bespoke/pkg/web"
	"github.com/microcosm-cc/bluemonday"
)

func safeMailReturnPath(value string) string {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return "/"
	}
	u, err := neturl.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	return u.RequestURI()
}

func draftLocation(id int64, returnTo string) string {
	location := fmt.Sprintf("/drafts/%d", id)
	if returnTo = safeMailReturnPath(returnTo); returnTo != "/" {
		location += "?return_to=" + neturl.QueryEscape(returnTo)
	}
	return location
}

func sanitizeMessageHTML(raw string) []byte {
	return bluemonday.UGCPolicy().SanitizeBytes([]byte(raw))
}

const htmlMessageDocumentStart = `<!doctype html><html><head><meta name="color-scheme" content="only light"><style>:root{color-scheme:only light}html,body{background:#fff;color:#171717}body{margin:0;padding:1px}</style></head><body>`

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed static/compose.js static/reader.js
var mailStaticFS embed.FS

func main() {
	migrations, _ := fs.Sub(migrationFS, "migrations")
	sqldb, err := db.Open("mail", migrations)
	if err != nil {
		log.Fatal(err)
	}
	vault, vaultErr := loadCredentialVault()
	if vaultErr != nil {
		log.Printf("mail account setup unavailable: %v", vaultErr)
	}
	googleConfig, googleErr := loadGoogleOAuthConfig()
	if googleErr != nil {
		log.Printf("Gmail account setup unavailable: %v", googleErr)
	}
	synchronizer := &mailSynchronizer{db: sqldb, vault: vault, google: googleConfig}
	type syncRequest struct {
		login     string
		accountID int64
	}
	syncRequests := make(chan syncRequest, 16)
	go func() {
		for request := range syncRequests {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := synchronizer.SyncAccount(ctx, request.login, request.accountID)
			cancel()
			if err != nil {
				log.Printf("mail sync account %d: %v", request.accountID, err)
			}
			web.Changed(request.login)
		}
	}()
	queueSync := func(login string, accountID int64) error {
		select {
		case syncRequests <- syncRequest{login: login, accountID: accountID}:
			return nil
		default:
			return errors.New("mail sync queue is busy")
		}
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rows, err := sqldb.QueryContext(ctx, `SELECT login, id FROM accounts
				WHERE status!='disabled' ORDER BY last_sync_at IS NOT NULL, last_sync_at, id`)
			if err != nil {
				log.Printf("mail background sync discovery: %v", err)
				cancel()
				continue
			}
			for rows.Next() {
				var login string
				var accountID int64
				if err := rows.Scan(&login, &accountID); err != nil {
					log.Printf("mail background sync scan: %v", err)
					break
				}
				if err := queueSync(login, accountID); err != nil {
					break // the next interval will pick up anything the queue cannot hold
				}
			}
			_ = rows.Close()
			cancel()
		}
	}()

	web.Run("mail", func(mux *http.ServeMux) {
		mux.HandleFunc("GET /_mail/compose.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			content, _ := mailStaticFS.ReadFile("static/compose.js")
			http.ServeContent(w, r, "compose.js", time.Time{}, bytes.NewReader(content))
		})
		mux.HandleFunc("GET /_mail/reader.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			content, _ := mailStaticFS.ReadFile("static/reader.js")
			http.ServeContent(w, r, "reader.js", time.Time{}, bytes.NewReader(content))
		})
		registerMailTools(mux, sqldb, synchronizer, queueSync)
		web.Live(mux, func(ctx context.Context, user auth.User) (templ.Component, error) {
			d, err := loadWorkspace(ctx, sqldb, user.Login, 0, 0)
			if err != nil {
				return nil, err
			}
			return views.Workspace(toWorkspaceView(d)), nil
		})
		web.Search(mux, func(ctx context.Context, user auth.User, query string) ([]web.SearchResult, error) {
			return searchMail(ctx, sqldb, user.Login, query)
		})
		web.DashboardCard(mux, func(ctx context.Context, user auth.User) (templ.Component, error) {
			var unread, healthy, total int
			err := sqldb.QueryRowContext(ctx, `SELECT
				COALESCE(SUM(CASE WHEN mb.role='inbox' THEN mb.unread_count ELSE 0 END), 0),
				COUNT(DISTINCT CASE WHEN a.status='healthy' THEN a.id END), COUNT(DISTINCT a.id)
				FROM accounts a LEFT JOIN mailboxes mb ON mb.account_id=a.id WHERE a.login=?`, user.Login).
				Scan(&unread, &healthy, &total)
			return views.DashCard(unread, healthy, total), err
		})

		web.Intent(mux, "Mail", web.IntentDef{
			Name: "compose-email", Title: "Compose Email",
			Prompt: "This text becomes the draft body. You will choose recipients before sending.",
			Handler: func(ctx context.Context, user auth.User, text string) (string, error) {
				result, err := sqldb.ExecContext(ctx, "INSERT INTO drafts (login, body) VALUES (?, ?)", user.Login, strings.TrimSpace(text))
				if err != nil {
					return "", err
				}
				id, err := result.LastInsertId()
				if err == nil {
					web.Changed(user.Login)
				}
				return fmt.Sprintf("/drafts/%d", id), err
			},
		})

		renderWorkspace := func(w http.ResponseWriter, r *http.Request, mailboxID, messageID int64) {
			user := auth.FromContext(r.Context())
			d, err := loadWorkspace(r.Context(), sqldb, user.Login, mailboxID, messageID)
			if err != nil {
				http.Error(w, "load mail", http.StatusInternalServerError)
				return
			}
			view := toWorkspaceView(d)
			view.CurrentPath = r.URL.RequestURI()
			views.Home(user, view).Render(r.Context(), w)
		}

		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			renderWorkspace(w, r, 0, 0)
		})
		mux.HandleFunc("GET /mailboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			renderWorkspace(w, r, id, 0)
		})
		mux.HandleFunc("GET /messages/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			user := auth.FromContext(r.Context())
			message, err := loadMessage(r.Context(), sqldb, user.Login, id)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if !message.BodyFetched {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				err = synchronizer.FetchBody(ctx, user.Login, id)
				cancel()
				if err != nil {
					log.Printf("mail fetch body message %d: %v", id, err)
				}
			}
			mailboxID, _ := strconv.ParseInt(r.URL.Query().Get("mailbox_id"), 10, 64)
			if mailboxID == 0 {
				mailboxID = message.MailboxID
			}
			var membership int
			err = sqldb.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM message_mailboxes mm
				JOIN mailboxes mb ON mb.id=mm.mailbox_id JOIN accounts a ON a.id=mb.account_id
				WHERE mm.message_id=? AND mm.mailbox_id=? AND a.login=?`, id, mailboxID, user.Login).Scan(&membership)
			if err != nil || membership == 0 {
				http.NotFound(w, r)
				return
			}
			renderWorkspace(w, r, mailboxID, id)
		})
		mux.HandleFunc("GET /messages/{id}/html", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			message, err := loadMessage(r.Context(), sqldb, auth.FromContext(r.Context()).Login, id)
			if err != nil || message.HTMLBody == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write([]byte(htmlMessageDocumentStart))
			_, _ = w.Write(sanitizeMessageHTML(message.HTMLBody))
			_, _ = w.Write([]byte(`<script>(()=>{const send=()=>parent.postMessage({type:"mail-html-height",height:document.documentElement.scrollHeight},"*");new ResizeObserver(send).observe(document.documentElement);addEventListener("load",send);send()})()</script>`))
			_, _ = w.Write([]byte(`</body></html>`))
		})
		mux.HandleFunc("POST /messages/{id}/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			accountID, err := queueMessageAction(r.Context(), sqldb, user.Login, messageID, r.PathValue("action"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			if err := queueSync(user.Login, accountID); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			web.Changed(user.Login)
			mailboxID, _ := strconv.ParseInt(r.FormValue("mailbox_id"), 10, 64)
			if r.PathValue("action") == "archive" || r.PathValue("action") == "trash" {
				http.Redirect(w, r, fmt.Sprintf("/mailboxes/%d", mailboxID), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/messages/%d", messageID), http.StatusSeeOther)
		})
		mux.HandleFunc("POST /messages/{id}/drafts/{mode}", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			message, err := loadMessage(r.Context(), sqldb, user.Login, messageID)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if !message.BodyFetched {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				err = synchronizer.FetchBody(ctx, user.Login, messageID)
				cancel()
				if err != nil {
					http.Error(w, "load message before responding", http.StatusUnprocessableEntity)
					return
				}
			}
			draftID, err := createResponseDraft(r.Context(), sqldb, user.Login, messageID, r.PathValue("mode"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, draftLocation(draftID, fmt.Sprintf("/messages/%d", messageID)), http.StatusSeeOther)
		})
		mux.HandleFunc("GET /attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			attachmentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			download, err := synchronizer.FetchAttachment(ctx, user.Login, attachmentID)
			cancel()
			if err != nil {
				http.Error(w, "download attachment", http.StatusUnprocessableEntity)
				return
			}
			w.Header().Set("Content-Type", download.ContentType)
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": download.Filename}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(w, r, download.Filename, time.Time{}, bytes.NewReader(download.Data))
		})

		renderCompose := func(w http.ResponseWriter, r *http.Request, d draft, problem string) {
			user := auth.FromContext(r.Context())
			accounts, err := loadAccounts(r.Context(), sqldb, user.Login)
			if err != nil {
				http.Error(w, "load sending accounts", http.StatusInternalServerError)
				return
			}
			views.Compose(user, toAccountViews(accounts), toDraftView(d), safeMailReturnPath(r.FormValue("return_to")), problem).Render(r.Context(), w)
		}
		mux.HandleFunc("GET /compose", func(w http.ResponseWriter, r *http.Request) {
			renderCompose(w, r, draft{Status: "draft"}, "")
		})
		mux.HandleFunc("GET /drafts", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			drafts, err := loadDraftSummaries(r.Context(), sqldb, user.Login)
			if err != nil {
				http.Error(w, "load drafts", http.StatusInternalServerError)
				return
			}
			out := make([]views.DraftSummary, 0, len(drafts))
			for _, item := range drafts {
				out = append(out, views.DraftSummary{ID: item.ID, To: item.To, Subject: item.Subject,
					Status: item.Status, Detail: item.Detail, UpdatedAt: item.UpdatedAt, AttachmentCount: item.AttachmentCount})
			}
			views.Drafts(user, out).Render(r.Context(), w)
		})
		mux.HandleFunc("GET /drafts/{id}", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			d, err := loadDraft(r.Context(), sqldb, user.Login, id)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			renderCompose(w, r, d, "")
		})
		saveDraftHandler := func(w http.ResponseWriter, r *http.Request, id int64) {
			user := auth.FromContext(r.Context())
			accountID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
			savedID, err := saveDraft(r.Context(), sqldb, user.Login, id, accountID,
				r.FormValue("to"), r.FormValue("cc"), r.FormValue("bcc"), r.FormValue("subject"), r.FormValue("body"))
			if err != nil {
				d := draft{ID: id, AccountID: accountID, To: []string{r.FormValue("to")},
					Cc: []string{r.FormValue("cc")}, Bcc: []string{r.FormValue("bcc")},
					Subject: r.FormValue("subject"), Body: r.FormValue("body"), Status: "draft"}
				renderCompose(w, r, d, err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, draftLocation(savedID, r.FormValue("return_to")), http.StatusSeeOther)
		}
		mux.HandleFunc("POST /drafts", func(w http.ResponseWriter, r *http.Request) {
			saveDraftHandler(w, r, 0)
		})
		mux.HandleFunc("POST /drafts/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			saveDraftHandler(w, r, id)
		})
		autosaveDraftHandler := func(w http.ResponseWriter, r *http.Request, id int64) {
			user := auth.FromContext(r.Context())
			accountID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
			savedID, err := saveDraft(r.Context(), sqldb, user.Login, id, accountID,
				r.FormValue("to"), r.FormValue("cc"), r.FormValue("bcc"), r.FormValue("subject"), r.FormValue("body"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			web.Changed(user.Login)
			w.Header().Set("Content-Type", "application/json")
			url := draftLocation(savedID, r.FormValue("return_to"))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": savedID, "url": url})
		}
		mux.HandleFunc("POST /drafts/autosave", func(w http.ResponseWriter, r *http.Request) {
			autosaveDraftHandler(w, r, 0)
		})
		mux.HandleFunc("POST /drafts/{id}/autosave", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			autosaveDraftHandler(w, r, id)
		})
		mux.HandleFunc("POST /drafts/{id}/attachments", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxDraftAttachmentBytes+(1<<20))
			err = r.ParseMultipartForm(1 << 20)
			if r.MultipartForm != nil {
				defer func() { _ = r.MultipartForm.RemoveAll() }()
			}
			var file multipart.File
			var header *multipart.FileHeader
			if err == nil {
				file, header, err = r.FormFile("attachment")
			}
			if err == nil {
				defer file.Close()
				err = addDraftAttachment(r.Context(), sqldb, user.Login, id, header.Filename,
					header.Header.Get("Content-Type"), file)
			}
			if err != nil {
				d, loadErr := loadDraft(r.Context(), sqldb, user.Login, id)
				if loadErr != nil {
					http.NotFound(w, r)
					return
				}
				renderCompose(w, r, d, err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, draftLocation(id, r.FormValue("return_to")), http.StatusSeeOther)
		})
		mux.HandleFunc("POST /drafts/{id}/attachments/{attachmentID}/remove", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, idErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
			attachmentID, attachmentErr := strconv.ParseInt(r.PathValue("attachmentID"), 10, 64)
			if idErr != nil || attachmentErr != nil {
				http.NotFound(w, r)
				return
			}
			if err := removeDraftAttachment(r.Context(), sqldb, user.Login, id, attachmentID); err != nil {
				http.NotFound(w, r)
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, draftLocation(id, r.FormValue("return_to")), http.StatusSeeOther)
		})
		mux.HandleFunc("POST /drafts/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if err := deleteDraft(r.Context(), sqldb, user.Login, id); err != nil {
				http.NotFound(w, r)
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, "/drafts", http.StatusSeeOther)
		})
		mux.HandleFunc("POST /drafts/{id}/send", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			err = synchronizer.SendDraft(ctx, user.Login, id)
			cancel()
			if err != nil {
				d, loadErr := loadDraft(r.Context(), sqldb, user.Login, id)
				if loadErr != nil {
					http.Error(w, "send draft", http.StatusUnprocessableEntity)
					return
				}
				renderCompose(w, r, d, err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, safeMailReturnPath(r.FormValue("return_to")), http.StatusSeeOther)
		})

		renderSettings := func(w http.ResponseWriter, r *http.Request, notice, problem string) {
			user := auth.FromContext(r.Context())
			accounts, err := loadAccounts(r.Context(), sqldb, user.Login)
			if err != nil {
				http.Error(w, "load accounts", http.StatusInternalServerError)
				return
			}
			views.AccountSettings(user, toAccountViews(accounts), vault != nil, googleConfig != nil, notice, problem).Render(r.Context(), w)
		}
		mux.HandleFunc("GET /settings/accounts", func(w http.ResponseWriter, r *http.Request) {
			renderSettings(w, r, r.URL.Query().Get("notice"), "")
		})
		mux.HandleFunc("GET /oauth/google/start", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			location, err := beginGoogleOAuth(r.Context(), sqldb, user.Login, googleConfig)
			if err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			http.Redirect(w, r, location, http.StatusTemporaryRedirect)
		})
		mux.HandleFunc("GET /oauth/google/callback", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			if providerError := r.URL.Query().Get("error"); providerError != "" {
				renderSettings(w, r, "", "Google authorization was cancelled: "+providerError)
				return
			}
			accountID, err := finishGoogleOAuth(r.Context(), sqldb, vault, googleConfig, user.Login,
				r.URL.Query().Get("state"), r.URL.Query().Get("code"))
			if err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			if err := queueSync(user.Login, accountID); err != nil {
				renderSettings(w, r, "Gmail connected", err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, "/settings/accounts?notice=Gmail+connected", http.StatusSeeOther)
		})
		mux.HandleFunc("POST /settings/accounts/icloud", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			if vault == nil {
				renderSettings(w, r, "", "Credential encryption is not configured.")
				return
			}
			accountID, err := addICloudAccount(r.Context(), sqldb, vault, user.Login, r.FormValue("email"), r.FormValue("password"))
			if err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			if err := queueSync(user.Login, accountID); err != nil {
				renderSettings(w, r, "Account added", err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, "/settings/accounts?notice=Account+added", http.StatusSeeOther)
		})
		mux.HandleFunc("POST /settings/accounts/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			var exists int
			if err := sqldb.QueryRowContext(r.Context(), "SELECT 1 FROM accounts WHERE id=? AND login=?", accountID, user.Login).Scan(&exists); err != nil {
				http.NotFound(w, r)
				return
			}
			if err := queueSync(user.Login, accountID); err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			web.Changed(user.Login)
			http.Redirect(w, r, "/settings/accounts?notice=Sync+requested", http.StatusSeeOther)
		})
		mux.HandleFunc("POST /settings/accounts/{id}/check", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			result, err := synchronizer.CheckAccount(ctx, user.Login, accountID)
			cancel()
			if err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			notice := fmt.Sprintf("%s connection passed: IMAP and SMTP authenticated; %d mailboxes visible.",
				strings.ToUpper(result.Provider), result.Mailboxes)
			renderSettings(w, r, notice, "")
		})
		mux.HandleFunc("POST /settings/accounts/{id}/disconnect", func(w http.ResponseWriter, r *http.Request) {
			user := auth.FromContext(r.Context())
			accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			removed, err := disconnectMailAccount(r.Context(), sqldb, user.Login, accountID,
				r.FormValue("confirm") == "disconnect")
			if err != nil {
				renderSettings(w, r, "", err.Error())
				return
			}
			web.Changed(user.Login)
			notice := fmt.Sprintf("Disconnected %s. Revoke provider access separately if desired.", removed.Email)
			renderSettings(w, r, notice, "")
		})
	})
}

func toAccountViews(accounts []account) []views.Account {
	out := make([]views.Account, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, views.Account{ID: a.ID, Provider: a.Provider, Email: a.Email,
			Status: a.Status, StatusDetail: a.StatusDetail, LastSyncAt: a.LastSyncAt})
	}
	return out
}

func toWorkspaceView(d workspaceData) views.WorkspaceData {
	out := views.WorkspaceData{Accounts: toAccountViews(d.Accounts), Live: d.Live}
	if d.MailboxID == 0 {
		out.CurrentMailbox = "All Inboxes"
	}
	for _, mb := range d.Mailboxes {
		out.Mailboxes = append(out.Mailboxes, views.Mailbox{ID: mb.ID, AccountName: mb.AccountName,
			Name: mb.Name, Role: mb.Role, Unread: mb.Unread, Selected: mb.ID == d.MailboxID})
		if mb.ID == d.MailboxID {
			out.CurrentMailbox = mb.Name
		}
	}
	for _, m := range d.Messages {
		out.Messages = append(out.Messages, toMessageView(m))
	}
	if d.Selected != nil {
		out.Selected = &views.MessageDetail{Message: toMessageView(d.Selected.messageSummary),
			To: d.Selected.To, Cc: d.Selected.Cc, TextBody: d.Selected.TextBody, HasHTMLBody: d.Selected.HTMLBody != ""}
		for _, item := range d.Selected.Attachments {
			out.Selected.Attachments = append(out.Selected.Attachments, views.Attachment{
				ID: item.ID, Filename: item.Filename, ContentType: item.ContentType, SizeBytes: item.SizeBytes,
			})
		}
	}
	return out
}

func toMessageView(m messageSummary) views.Message {
	return views.Message{ID: m.ID, MailboxID: m.MailboxID, FromName: m.FromName,
		FromAddress: m.FromAddress, Subject: m.Subject, Preview: m.Preview, ReceivedAt: m.ReceivedAt,
		IsRead: m.IsRead, IsStarred: m.IsStarred, HasAttachments: m.HasAttachments}
}

func toDraftView(d draft) views.Draft {
	out := views.Draft{ID: d.ID, AccountID: d.AccountID, To: strings.Join(d.To, ", "),
		Cc: strings.Join(d.Cc, ", "), Bcc: strings.Join(d.Bcc, ", "), Subject: d.Subject,
		Body: d.Body, Status: d.Status, Detail: d.Detail}
	for _, item := range d.Attachments {
		out.Attachments = append(out.Attachments, views.DraftAttachment{ID: item.ID, Filename: item.Filename, SizeBytes: item.SizeBytes})
	}
	return out
}
