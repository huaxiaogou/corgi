package wecom

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	pendingUserTTL      = 15 * time.Minute
	pendingHintCooldown = 60 * time.Second
	maxPendingUsers     = 8
)

// PendingUser represents an unallowlisted WeCom single-chat sender awaiting one-click approval in Web UI.
// WeCom smart bots emit anonymized T-prefixed userids per (bot, member) that cannot be guessed from names.
type PendingUser struct {
	UserID  string    `json:"user_id"`
	Preview string    `json:"preview,omitempty"`
	At      time.Time `json:"at"`
}

func (c *Client) recordPendingUser(ctx context.Context, userID, text string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	now := time.Now()
	preview := trimPreview(text, 32)
	shouldHint := false

	c.mu.Lock()
	if c.pendingUsers == nil {
		c.pendingUsers = map[string]PendingUser{}
	}
	if c.lastHintAt == nil {
		c.lastHintAt = map[string]time.Time{}
	}
	c.pruneExpiredPendingLocked(now)
	c.pendingUsers[userID] = PendingUser{UserID: userID, Preview: preview, At: now}
	c.evictOverflowPendingLocked()

	if c.online.Load() {
		if last, ok := c.lastHintAt[userID]; !ok || now.Sub(last) >= pendingHintCooldown {
			c.lastHintAt[userID] = now
			shouldHint = true
		}
	}
	c.mu.Unlock()

	if shouldHint {
		go func() {
			hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			msg := fmt.Sprintf("尚未绑定 Corgi 权限：请打开 Corgi 网页「自动化与设置 → 企微与偏好」点一下确认授权（ID: `%s`）。", userID)
			if err := c.SendMarkdown(hctx, userID, msg); err != nil {
				c.logger.Debug("send unbound wecom hint failed", "user", userID, "err", err)
			}
		}()
	}
}

// PendingUsers returns active unallowlisted single-chat senders (newest first) awaiting approval.
func (c *Client) PendingUsers() []PendingUser {
	now := time.Now()
	c.mu.Lock()
	c.pruneExpiredPendingLocked(now)
	c.pruneAllowedPendingLocked()
	out := make([]PendingUser, 0, len(c.pendingUsers))
	for _, p := range c.pendingUsers {
		out = append(out, p)
	}
	c.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// DismissPending removes a pending sender from the approval queue.
func (c *Client) DismissPending(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	c.mu.Lock()
	delete(c.pendingUsers, userID)
	delete(c.lastHintAt, userID)
	c.mu.Unlock()
}

func (c *Client) pruneExpiredPendingLocked(now time.Time) {
	for id, p := range c.pendingUsers {
		if now.Sub(p.At) > pendingUserTTL {
			delete(c.pendingUsers, id)
		}
	}
	for id, ts := range c.lastHintAt {
		if now.Sub(ts) > pendingUserTTL {
			delete(c.lastHintAt, id)
		}
	}
}

func (c *Client) pruneAllowedPendingLocked() {
	for id := range c.pendingUsers {
		if slices.Contains(c.cfg.AllowUsers, id) {
			delete(c.pendingUsers, id)
			delete(c.lastHintAt, id)
		}
	}
}

func (c *Client) evictOverflowPendingLocked() {
	for len(c.pendingUsers) > maxPendingUsers {
		var oldestID string
		var oldestAt time.Time
		for id, p := range c.pendingUsers {
			if oldestID == "" || p.At.Before(oldestAt) {
				oldestID = id
				oldestAt = p.At
			}
		}
		if oldestID == "" {
			break
		}
		delete(c.pendingUsers, oldestID)
	}
}

func trimPreview(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
