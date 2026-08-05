package views

import "time"

type Draft struct {
	ID          int64
	AccountID   int64
	To          string
	Cc          string
	Bcc         string
	Subject     string
	Body        string
	Status      string
	Detail      string
	Attachments []DraftAttachment
}

type DraftAttachment struct {
	ID        int64
	Filename  string
	SizeBytes int64
}

type DraftSummary struct {
	ID              int64
	To              []string
	Subject         string
	Status          string
	Detail          string
	UpdatedAt       time.Time
	AttachmentCount int
}

type Account struct {
	ID           int64
	Provider     string
	Email        string
	Status       string
	StatusDetail string
	LastSyncAt   *time.Time
}

type Mailbox struct {
	ID          int64
	AccountName string
	Name        string
	Role        string
	Unread      int
	Selected    bool
}

type Message struct {
	ID             int64
	MailboxID      int64
	FromName       string
	FromAddress    string
	Subject        string
	Preview        string
	ReceivedAt     time.Time
	IsRead         bool
	IsStarred      bool
	HasAttachments bool
}

type MessageDetail struct {
	Message
	To          []string
	Cc          []string
	TextBody    string
	HasHTMLBody bool
	Attachments []Attachment
}

type Attachment struct {
	ID          int64
	Filename    string
	ContentType string
	SizeBytes   int64
}

type WorkspaceData struct {
	Accounts       []Account
	Mailboxes      []Mailbox
	Messages       []Message
	Selected       *MessageDetail
	Live           bool
	CurrentPath    string
	CurrentMailbox string
}

func sender(m Message) string {
	if m.FromName != "" {
		return m.FromName
	}
	if m.FromAddress != "" {
		return m.FromAddress
	}
	return "Unknown sender"
}

func subject(value string) string {
	if value == "" {
		return "(No subject)"
	}
	return value
}

func shortTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format("Jan 2")
}

func readAction(m *MessageDetail) string {
	if m.IsRead {
		return "mark-unread"
	}
	return "mark-read"
}

func readActionLabel(m *MessageDetail) string {
	if m.IsRead {
		return "Mark unread"
	}
	return "Mark read"
}

func starAction(m *MessageDetail) string {
	if m.IsStarred {
		return "unstar"
	}
	return "star"
}

func starActionLabel(m *MessageDetail) string {
	if m.IsStarred {
		return "Remove star"
	}
	return "Star"
}
