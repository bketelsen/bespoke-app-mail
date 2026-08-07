# Mail — a Bespoke app

A private, local-first email client for the owner's Gmail and Apple iCloud
accounts. It borrows Apple Mail's quiet three-region information architecture
without trying to reproduce Apple's pixels.

This is an **unofficial, third-party app** for [Bespoke](https://github.com/bketelsen/bespoke). Nobody vetted it but its
author. Read [What it does with your stuff](#what-it-does-with-your-stuff)
before installing it, the same as any code you run as yourself.

## Install

From your Bespoke instance directory, with the platform at v0.13.0 or newer:

```sh
go tool bespoke add mail
```

That pins this module, writes `apps/mail/app.toml` with a free port, and
recompiles your instance stylesheet. Pin a version you have actually read with
`bespoke add mail@v0.1.0`.

The directory must be named `mail`: the slug names this app's database,
process, and subdomain, and the source picks it.

## What it does with your stuff

- **Stores** in its own SQLite database: messages, mailboxes, contacts, drafts,
  and your account credentials. Credentials are encrypted with
  `BESPOKE_MAIL_KEY`. Attachments are written as files under the app's data
  directory.
- **Talks to** the mail providers you connect: Gmail (`imap.gmail.com`,
  `smtp.gmail.com`, `gmail.googleapis.com`, `oauth2.googleapis.com`) and iCloud
  (`imap.mail.me.com`, `smtp.mail.me.com`). Remote images in HTML messages are
  not loaded.
- **Publishes** events and in-app notifications to your own Bespoke instance —
  the exact types, fields, and suppression rules are in
  [Events and automation](#events-and-automation). They carry sender, subject,
  and status detail — never message bodies or credentials.
- **Never** sends your mail anywhere but the provider it came from.

Set `BESPOKE_MAIL_KEY` (32 random bytes, base64) and the Google OAuth client
values in `~/bespoke/env.d/mail` on the app host — never the shared env file, or
every other app can read your mail key.

This app holds more of your private data than anything else you are likely to
install. Read the source before you trust it.

Like every Bespoke app it runs as your user, so nothing above is *enforced* by
the platform — it is a description you can check against the source
([ADR-0031](https://github.com/bketelsen/bespoke/blob/main/docs/adr/0031-third-party-app-packages.md)).

## Events and automation

The app publishes domain events to your instance's platformd, which stores
them durably, shows their notifications in the platform inbox, and matches
automation rules against them
([ADR-0035](https://github.com/bketelsen/bespoke/blob/main/docs/adr/0035-durable-events-notifications-automations.md)).
Rules match on the exact `type` string and on condition paths under `data`, so
the names below are the contract. Events publish only after the local write
committed; a failed publish is logged and never fails or retries the sync or
send that produced it.

| Type | Fires when | Notification | `data` fields |
| --- | --- | --- | --- |
| `mail.received` | a sync stores a new, unread message in an inbox mailbox | first 10 per mailbox per sync | `message_id`, `account`, `from`, `from_name`, `subject`, `has_attachments` |
| `mail.account_sync_failed` | an account's sync newly fails (non-error → error) | always | `account_id`, `email`, `provider`, `detail` |
| `mail.draft_send_failed` | building or submitting a draft over SMTP fails | always | `draft_id`, `to`, `subject`, `error` |
| `mail.sent` | a draft is accepted by SMTP and marked sent | never — silent | `draft_id`, `account`, `to`, `subject`, `detail` (when present) |

Every event also carries the standard envelope: `id`, `type`, `subject_id`
(the message, account, or draft ID as a string), `occurred_at`, `data`, and a
`source` of `mail`.

What a rule can rely on:

- **`mail.received` means genuinely new mail, not cache churn.** It fires only
  when a sync inserts a message row that did not exist locally, only for
  mailboxes whose role is `inbox`, and only when the message is still unread.
  Flag changes, moves, and re-fetches of known messages never fire it.
- **Backfill is silent.** A mailbox with no UID high-water mark — its first
  sync, or a cache rebuild after the server changes UIDVALIDITY — treats every
  message as new, so it publishes nothing at all. The suppression is per
  mailbox, so connecting an account never floods the event log with history.
- **Notifications are capped; events are not.** The first 10 new messages per
  mailbox per sync attach a notification — title `New message from <sender>`
  (`from_name`, falling back to the address), body is the subject, path
  `/messages/<message_id>`, group key `mail:<account email>` — and the rest of
  that sync's new messages still publish, silently.
- **`has_attachments` is always `false` at publish time.** Only the envelope
  has been fetched when the event fires, so do not condition on it.
- **`mail.account_sync_failed` fires once per outage.** It publishes only on
  the transition into the `error` status — a Google token-refresh failure
  counts — so the 5-minute background poller cannot re-notify while an account
  stays broken; it can fire again only after a successful sync resets the
  status. `detail` is the failure text truncated to 300 characters, and the
  notification links to `/settings/accounts` under group key
  `mail:sync:<email>`.
- **`mail.draft_send_failed` fires on every failed attempt** — it is not
  transition-gated. `to` is the draft's list of To addresses and `error` the
  truncated failure text. The notification (title `Message failed to send`)
  links back to `/drafts/<draft_id>`.
- **`mail.sent` is silent** and publishes only after the draft row reaches
  `sent`. `data.detail` appears only when SMTP accepted the message but the
  iCloud Sent-mailbox copy could not be filed; Gmail retains its own copy, so
  Gmail sends never carry it.

Notification titles are truncated to 120 bytes and bodies to 500, on rune
boundaries.

### Tools available to automation

Only the read-only tools opt into automation rules: `list_mail_accounts`,
`search_mail`, `get_mail_message`, `list_mail_drafts`, `get_mail_draft`,
`sync_accounts`, and `check_mail_account`. Every mutating or destructive tool
— `create_draft`, `create_response_draft`, `update_draft`,
`add_draft_text_attachment`, `remove_draft_attachment`, `delete_draft`,
`send_draft`, `update_message`, `bulk_update_messages`, and
`disconnect_mail_account` — declares no automation policy, so the platform
refuses to run it from a rule. An automation can read, search, and check your
mail and queue a sync; it can never write, send, or delete anything.

## Spec

The behavior this app is built to, unchanged from its original private build.

### Usage moment

Open Mail to triage all personal inboxes in one place, read and search cached
mail, and write or reply without switching providers. The application runs on
the private Bespoke host and remains useful while either provider is briefly
unavailable.

### Version 1

### Accounts and authentication

- Multiple accounts per Bespoke user; provider is explicitly `gmail` or
  `icloud`.
- Gmail connects to IMAP and SMTP with Google OAuth 2.0 and SASL XOAUTH2. OAuth
  client configuration comes from environment variables; refresh tokens are
  encrypted at rest.
- iCloud connects to `imap.mail.me.com:993` and `smtp.mail.me.com:587` using an
  Apple app-specific password, encrypted at rest. The Apple Account password is
  never accepted.
- Credential encryption uses a dedicated deployment key supplied through the
  environment. Account setup is unavailable—not silently insecure—when the key
  is absent. Secrets, tokens, and message bodies never appear in logs.
- Every account, mailbox, message, draft, and operation is scoped to the
  authenticated Bespoke login.
- Disconnecting an account requires explicit confirmation and removes its
  encrypted credentials, cached remote mail, and queued operations. Unsent
  local drafts remain with no sending account selected. Google authorization
  or an Apple app-specific password must be revoked separately at the provider.

Runtime configuration belongs in the uncommitted `deploy/deploy.env`:

```text
BESPOKE_MAIL_KEY=<output of: openssl rand -base64 32>
BESPOKE_MAIL_GOOGLE_CLIENT_ID=<Google OAuth web client ID>
BESPOKE_MAIL_GOOGLE_CLIENT_SECRET=<Google OAuth web client secret>
BESPOKE_MAIL_GOOGLE_REDIRECT_URL=https://<mail-host>/oauth/google/callback
```

The Google OAuth client must allow that exact redirect URL. iCloud credentials
are entered per user in Mail settings and must be an app-specific password.

### Synchronization

- SQLite is the local read model and source of drafts/pending operations; the
  IMAP server remains authoritative for remote mail state.
- Sync discovers mailboxes and reconciles the newest 100 messages in up to 20
  folders per account, prioritizing inbox, sent, drafts, archive, trash, and
  spam. This refreshes flags and removes stale cached rows without downloading
  an unbounded account.
- Subsequent syncs track mailbox UID validity and highest UID. A UIDVALIDITY
  change safely rebuilds that mailbox's cache.
- Inbox, sent, drafts, archive/all-mail, trash, spam, and provider/user folders
  are represented through provider-normalized mailbox roles.
- Background polling and a manual Sync action update the cache. IMAP IDLE is a
  later optimization; correctness cannot depend on a permanent connection.
- Read/unread, star/unstar, archive, move, and trash actions are written as
  pending operations, applied remotely, and retried after transient failures.
  The UI shows pending or failed state instead of pretending remote success.
- Attachments are metadata-only until explicitly downloaded. Attachment bytes
  are not stored in SQLite.

### Reading and composing

- Unified inbox plus account and mailbox navigation.
- Message list shows sender, subject, preview, timestamp, unread/star state, and
  attachment presence.
- Reader shows participants, dates, sanitized HTML when available, safe
  plain-text fallback content, attachments, and reply/forward/archive/trash
  controls. HTML is isolated in a sandboxed frame; scripts, forms, and remote
  resources are blocked, so opening a message does not load tracking pixels.
- Compose, reply, reply-all, and forward support plain text, To/Cc/Bcc, subject,
  debounced autosaved local drafts, explicit draft deletion, and attachments
  within a configured size limit. Sending waits for any pending autosave.
- Sending occurs through the selected account's SMTP service. A sent copy is
  reconciled with the provider-specific Sent mailbox without duplicating Gmail's
  automatically retained copy.

### Search and Bespoke surfaces

- Local search covers sender/recipient names and addresses, subject, preview,
  and cached plain-text body. Results deep-link to messages and never contact a
  provider or LLM.
- The dashboard card shows unread unified-inbox count, account health, and last
  successful sync using cheap local queries.
- Account settings can verify credential decryption, token refresh, IMAP login
  and mailbox listing, and SMTP authentication without selecting message
  content or submitting a message for delivery.
- Live regions update after sync and every local or remote mutation.
- Tools expose account discovery and connection checks, local message search,
  safe plain-text message reading, draft listing and review, new and
  reply/reply-all/forward draft creation, draft editing, generated text
  attachment management, message flag changes, and explicit draft sending.
  Sending and destructive actions state that they only run on an explicit
  request.
- Agentic triage may batch one action across at most 50 exact message IDs. The
  local optimistic changes and pending operations commit atomically, and remote
  synchronization is queued once per affected account; broad query-based
  deletion without reviewing concrete matches is not exposed.
- The `compose-email` text intent opens a draft with the supplied text as its
  body; recipients remain an explicit user choice.
- In-app LLM chat is out of scope for v1: automatically stuffing private mail
  into model context is too broad. Narrow, user-invoked mail assistance can be
  designed later.

### Views and responsive behavior

The desktop workspace uses the wide AppShell and three regions:

1. A compact mailbox/account sidebar.
2. A bounded message list with independent scrolling.
3. A flexible reader or composer with independent scrolling.

At large widths all three regions may be visible. At intermediate widths the
mailbox sidebar remains compact or becomes an explicit drawer while list and
reader share the workspace. At phone width, mailbox list, message list, reader,
and composer are separate URL-addressable screens with normal browser history
and visible back navigation. No essential action depends on hover, drag, a fine
pointer, or a viewport wider than 375px. Selection and focus are restored after
navigation or live patches.

Primary routes:

- `/` — unified inbox or account-setup empty state
- `/mailboxes/{id}` — one mailbox's message list
- `/messages/{id}` — reader
- `/compose` and `/drafts/{id}` — composer
- `/settings/accounts` — account health and setup
- `/oauth/google/start` and `/oauth/google/callback` — Gmail authorization

### Data model

- `accounts`: provider identity, encrypted credential material, health, and
  sync timestamps.
- `mailboxes`: remote identity, normalized role, UID validity/high-water mark,
  counts, and ordering.
- `messages`: stable local identity plus remote UID, envelope, flags, threading
  headers, preview, safe text, and attachment indicator.
- `message_mailboxes`: mailbox membership, supporting Gmail's label semantics.
- `addresses`: ordered From/To/Cc/Bcc/Reply-To values per message.
- `attachments`: MIME metadata and remote part identity; bytes are fetched on
  demand.
- `drafts` and `draft_attachments`: local compose state.
- `pending_operations`: durable, retryable remote mutations.
- `oauth_states`: short-lived, one-use Google OAuth state.

### Publication and retention boundaries

- The app does not become an email server and accepts no inbound SMTP.
- Deleting a local account removes its local cache and credentials only after an
  explicit confirmation; it does not delete provider mail.
- Cached mail follows the Bespoke SQLite backup policy. Downloaded attachment
  files are disposable cache and must not be required for recovery.

### Non-goals for version 1

- Calendar, contacts, arbitrary IMAP providers, Exchange, or POP.
- Rules, server-side filters, signatures, vacation responders, aliases, S/MIME,
  PGP, read receipts, scheduled send, snooze, or undo-send.
- Automatically loading remote images.
- Full historical offline mirroring on first setup.
- A general mail server, public signup, or sharing accounts between Bespoke
  users.

Calendar is the intended next product area, but it will use separate provider
adapters and tables rather than being smuggled into the mail sync model.

## Developing

`views/*_templ.go` is committed on purpose — instances build this module
straight out of the read-only Go module cache and never run `templ generate`
over it. After editing a `.templ`, run `just ui` and commit the output.

## License

MIT — see [LICENSE](LICENSE).
