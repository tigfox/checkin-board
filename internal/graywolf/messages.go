package graywolf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxMessageText is graywolf's default DM text cap (APRS101).
	MaxMessageText = 67
	// MaxMessageTextUnsafe is the ceiling an operator override may raise it to.
	MaxMessageTextUnsafe = 200

	maxListLimit    = 500
	maxCatchUpPages = 100
)

// ErrCatchUpIncomplete means CatchUp hit its page bound before the
// inbox ran dry; call again from the returned cursor.
var ErrCatchUpIncomplete = errors.New("graywolf: catch-up page limit reached")

// ErrCursorStalled means graywolf returned a non-empty page without a
// new cursor, so paging can't advance.
var ErrCursorStalled = errors.New("graywolf: message cursor did not advance")

// addresseeRe accepts a callsign with optional SSID, or a tactical label.
var addresseeRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-[A-Z0-9]{1,2})?$|^[A-Z0-9][A-Z0-9-]{0,8}$`)

var validSendPaths = map[string]bool{"": true, "rf_only": true, "is_only": true, "both": true}

func normalizeAddressee(to string) (string, error) {
	call := strings.ToUpper(strings.TrimSpace(to))
	if !addresseeRe.MatchString(call) {
		return "", fmt.Errorf("graywolf: %q is not a valid APRS addressee", to)
	}
	return call, nil
}

// validateText rejects text graywolf or the APRS message format can't
// carry: empty, too long, control characters, and the msgid/reply-ack
// delimiters '{', '|', '~'.
func validateText(text string) error {
	if text == "" {
		return errors.New("graywolf: message text is empty")
	}
	if len(text) > MaxMessageTextUnsafe {
		return fmt.Errorf("graywolf: message text is %d chars, max %d", len(text), MaxMessageTextUnsafe)
	}
	for _, r := range text {
		if r < 0x20 || r > 0x7e || r == '{' || r == '|' || r == '~' {
			return fmt.Errorf("graywolf: message text contains unsupported character %q", r)
		}
	}
	return nil
}

func messagePath(id uint64, suffix string) (string, error) {
	if id == 0 {
		return "", errors.New("graywolf: message id must be non-zero")
	}
	return "/api/messages/" + strconv.FormatUint(id, 10) + suffix, nil
}

// SendMessage queues a DM via POST /api/messages. graywolf assigns the
// msgid; the returned Message carries it and the row id for resends.
func (c *Client) SendMessage(ctx context.Context, req SendRequest) (Message, error) {
	to, err := normalizeAddressee(req.To)
	if err != nil {
		return Message{}, err
	}
	if err := validateText(req.Text); err != nil {
		return Message{}, err
	}
	body := req
	body.To = to
	var out Message
	err = c.do(ctx, http.MethodPost, "/api/messages", nil, body, &out)
	return out, err
}

// GetMessage calls GET /api/messages/{id}.
func (c *Client) GetMessage(ctx context.Context, id uint64) (Message, error) {
	path, err := messagePath(id, "")
	if err != nil {
		return Message{}, err
	}
	var out Message
	err = c.do(ctx, http.MethodGet, path, nil, nil, &out)
	return out, err
}

// ResendMessage calls POST /api/messages/{id}/resend, which retransmits
// the row with its existing msgid. A 409 (IsConflict) means a send of
// that row is already in flight.
func (c *Client) ResendMessage(ctx context.Context, id uint64) (Message, error) {
	path, err := messagePath(id, "/resend")
	if err != nil {
		return Message{}, err
	}
	var out Message
	err = c.do(ctx, http.MethodPost, path, nil, nil, &out)
	return out, err
}

// DeleteMessage soft-deletes one row via DELETE /api/messages/{id}.
func (c *Client) DeleteMessage(ctx context.Context, id uint64) error {
	path, err := messagePath(id, "")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// MarkRead calls POST /api/messages/{id}/read.
func (c *Client) MarkRead(ctx context.Context, id uint64) error {
	path, err := messagePath(id, "/read")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, nil, nil, nil)
}

// ListMessages fetches one page of GET /api/messages.
func (c *Client) ListMessages(ctx context.Context, p ListParams) (MessagePage, error) {
	q, err := p.query()
	if err != nil {
		return MessagePage{}, err
	}
	var out MessagePage
	err = c.do(ctx, http.MethodGet, "/api/messages", q, nil, &out)
	return out, err
}

func (p ListParams) query() (url.Values, error) {
	if p.Limit < 0 || p.Limit > maxListLimit {
		return nil, fmt.Errorf("graywolf: list limit %d out of range 0..%d (0 = server default)", p.Limit, maxListLimit)
	}
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("folder", p.Folder)
	set("peer", strings.ToUpper(p.Peer))
	set("thread_kind", p.ThreadKind)
	set("thread_key", strings.ToUpper(p.ThreadKey))
	set("cursor", p.Cursor)
	if !p.Since.IsZero() {
		q.Set("since", p.Since.UTC().Format(time.RFC3339))
	}
	if p.UnreadOnly {
		q.Set("unread_only", "true")
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q, nil
}

// CatchUp pages forward from p.Cursor, calling fn for every change,
// until a page comes back empty. It returns the cursor to resume from:
// the last cursor whose page fn handled completely. graywolf returns an
// empty cursor for an empty page, so the previous one is kept.
//
// graywolf orders this feed by updated_at, so a row re-appears when it
// changes (e.g. after MarkRead). fn must be idempotent per row id.
func (c *Client) CatchUp(ctx context.Context, p ListParams, fn func(MessageChange) error) (string, error) {
	cursor := p.Cursor
	for range maxCatchUpPages {
		page, err := c.ListMessages(ctx, withCursor(p, cursor))
		if err != nil {
			return cursor, err
		}
		if len(page.Changes) == 0 {
			return cursor, nil
		}
		for _, ch := range page.Changes {
			if err := fn(ch); err != nil {
				return cursor, err
			}
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return cursor, ErrCursorStalled
		}
		cursor = page.Cursor
	}
	return cursor, ErrCatchUpIncomplete
}

func withCursor(p ListParams, cursor string) ListParams {
	next := p
	next.Cursor = cursor
	return next
}

// MessagePreferences calls GET /api/messages/preferences.
func (c *Client) MessagePreferences(ctx context.Context) (MessagePreferences, error) {
	var out MessagePreferences
	err := c.do(ctx, http.MethodGet, "/api/messages/preferences", nil, nil, &out)
	return out, err
}

// MaxText is the DM text length graywolf will accept: the operator
// override when it is in graywolf's valid range, otherwise 67.
func (p MessagePreferences) MaxText() int {
	if p.MaxMessageTextOverride > MaxMessageText && p.MaxMessageTextOverride <= MaxMessageTextUnsafe {
		return p.MaxMessageTextOverride
	}
	return MaxMessageText
}

func prefsPath(kind, key string) (string, error) {
	if kind != ThreadKindDM && kind != ThreadKindTactical {
		return "", fmt.Errorf("graywolf: thread kind %q must be dm or tactical", kind)
	}
	k, err := normalizeAddressee(key)
	if err != nil {
		return "", err
	}
	return "/api/messages/conversations/" + kind + "/" + k + "/prefs", nil
}

// ConversationPrefs reads the per-conversation override. graywolf
// returns defaults (wait_for_ack=true) when none is stored.
func (c *Client) ConversationPrefs(ctx context.Context, kind, key string) (ConversationPrefs, error) {
	path, err := prefsPath(kind, key)
	if err != nil {
		return ConversationPrefs{}, err
	}
	var out ConversationPrefs
	err = c.do(ctx, http.MethodGet, path, nil, nil, &out)
	return out, err
}

// SetConversationPrefs replaces the per-conversation override. Both
// fields are always sent: graywolf treats the body as the full state.
func (c *Client) SetConversationPrefs(ctx context.Context, kind, key string, p ConversationPrefs) (ConversationPrefs, error) {
	path, err := prefsPath(kind, key)
	if err != nil {
		return ConversationPrefs{}, err
	}
	if !validSendPaths[p.SendPath] {
		return ConversationPrefs{}, fmt.Errorf("graywolf: send_path %q is not one of (empty)|rf_only|is_only|both", p.SendPath)
	}
	body := ConversationPrefs{SendPath: p.SendPath, WaitForAck: p.WaitForAck}
	var out ConversationPrefs
	err = c.do(ctx, http.MethodPut, path, nil, body, &out)
	return out, err
}
