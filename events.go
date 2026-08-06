package main

import (
	"context"
	"log"
	"time"
	"unicode/utf8"

	"github.com/bketelsen/bespoke/pkg/auth"
	"github.com/bketelsen/bespoke/pkg/events"
	"github.com/google/uuid"
)

const (
	maxNotificationTitleBytes = 120
	maxNotificationBodyBytes  = 500
	maxNewMailNotifications   = 10
)

// publishMailEvent reports a domain event to platformd after the local
// mutation committed. Publication failures are logged and never fail the
// request or sync that produced them.
func publishMailEvent(ctx context.Context, login string, event events.Event) {
	event.ID = uuid.NewString()
	event.OccurredAt = time.Now().UTC()
	if err := events.New("mail").Publish(ctx, auth.User{Login: login}, event); err != nil {
		log.Printf("mail event publish %s: %v", event.Type, err)
	}
}

// truncateBytes shortens s to at most limit UTF-8 bytes on a rune boundary.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
