package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
)

// The rest of the work on one ticket: side conversations, merging, editing and
// deleting notes, the activity log and saved replies. Like every tool, each runs
// through the public API as the caller (contracts/agent.md "opendesk MCP server").

const sideArg = "The side conversation's id, from get_ticket's side_conversations."

func ticketWorkTools() []*tool {
	return []*tool{
		{
			Name: "start_side_conversation", Title: "Start a side conversation", Scopes: []string{auth.ScopeTicketsWrite}, OpenWorld: true,
			Description: "Email someone outside the team about a ticket, such as a supplier, a partner or another department, without the customer seeing it. " +
				"The conversation stays on the ticket for the team, and answers come back into it. The first message is emailed from the ticket's reply " +
				"address (or mailbox_id). Nothing of the customer's conversation goes along unless include_history is true. Write the body in markdown, without a signature.",
			Schema: obj(map[string]any{
				"ticket":          str(ticketArg),
				"to":              arr("Recipients' email addresses.", str(""), 20),
				"cc":              arr("Email addresses to copy.", str(""), 20),
				"subject":         strN("The email subject.", 1, 500),
				"body":            strN("The first message, in markdown.", 1, maxBodyChars),
				"include_history": boolean("Quote the ticket's conversation with the customer (its last 10 public replies, never notes) under the first message."),
				"mailbox_id":      str("Send from this mailbox of the ticket's brand instead of the ticket's reply address."),
			}, "ticket", "to", "subject", "body"),
			Run: startSideConversation,
		},
		{
			Name: "reply_to_side_conversation", Title: "Write in a side conversation", Scopes: []string{auth.ScopeTicketsWrite}, OpenWorld: true,
			Description: "Email the next message of a side conversation to its recipients. The customer never sees it. A closed side conversation opens again. " +
				"Markdown body, without a signature.",
			Schema: obj(map[string]any{
				"ticket":            str(ticketArg),
				"side_conversation": str(sideArg),
				"body":              strN("The message, in markdown.", 1, maxBodyChars),
			}, "ticket", "side_conversation", "body"),
			Run: replyToSideConversation,
		},
		{
			Name: "update_side_conversation", Title: "Close or reopen a side conversation", Scopes: []string{auth.ScopeTicketsWrite}, Idempotent: true,
			Description: "Close a side conversation when it is done, or open it again. Nothing is emailed.",
			Schema: obj(map[string]any{
				"ticket":            str(ticketArg),
				"side_conversation": str(sideArg),
				"status":            enum("open or closed.", "open", "closed"),
			}, "ticket", "side_conversation", "status"),
			Run: updateSideConversation,
		},
		{
			Name: "merge_tickets", Title: "Merge tickets", Scopes: []string{auth.ScopeTicketsWrite}, Destructive: true,
			Description: "Merge a ticket into another: every message and label moves into the target, and the merged ticket is closed. " +
				"Use it for duplicates of the same request. Returns the target ticket.",
			Schema: obj(map[string]any{
				"ticket": str("The ticket to merge away: its number like #123 (or 123), or its id."),
				"into":   str("The ticket to keep: its number like #123 (or 123), or its id."),
			}, "ticket", "into"),
			Run: mergeTickets,
		},
		{
			Name: "edit_note", Title: "Edit an internal note", Scopes: []string{auth.ScopeTicketsWrite}, Person: true, Destructive: true, Idempotent: true,
			Description: "Replace the text of one of your own internal notes. Messages of side conversations were emailed and cannot be edited. " +
				"Offered to connections that sign in as you, not to API keys. Markdown body.",
			Schema: obj(map[string]any{
				"ticket":     str(ticketArg),
				"message_id": str("The note's id, from get_ticket's messages."),
				"body":       strN("The new text, in markdown.", 1, maxBodyChars),
			}, "ticket", "message_id", "body"),
			Run: editNote,
		},
		{
			Name: "delete_note", Title: "Delete an internal note", Scopes: []string{auth.ScopeTicketsWrite}, Person: true, Destructive: true, Idempotent: true,
			Description: "Delete an internal note. It leaves a marker in the conversation, and admins can restore it in the app. " +
				"Which notes you may delete follows the workspace's rules (your own, or all for admins). Only internal notes: replies and side conversation " +
				"messages are refused. Offered to connections that sign in as you, not to API keys.",
			Schema: obj(map[string]any{
				"ticket":     str(ticketArg),
				"message_id": str("The note's id, from get_ticket's messages."),
			}, "ticket", "message_id"),
			Run: deleteNote,
		},
		{
			Name: "get_ticket_activity", Title: "Get a ticket's activity", Scopes: []string{auth.ScopeTicketsRead}, ReadOnly: true, Idempotent: true,
			Description: "Read what happened to a ticket besides its messages, newest last: status, priority, assignee, team and label changes, merges, " +
				"snoozes, side conversations, automations and who or what did each.",
			Schema: obj(map[string]any{
				"ticket": str(ticketArg),
				"limit":  integer("How many of the latest entries to return.", 1, maxActivities, 50),
			}, "ticket"),
			Run: getTicketActivity,
		},
		{
			Name: "list_saved_replies", Title: "List saved replies", Scopes: []string{auth.ScopeTicketsRead}, ReadOnly: true, Idempotent: true,
			Description: "Read the team's saved replies (canned answers) as plain text, to answer the way the team does. With ticket, only the ones " +
				"offered on that ticket; with query, the ones whose title or shortcut match.",
			Schema: obj(map[string]any{
				"query":  strN("Words in the title or shortcut.", 1, 200),
				"ticket": str("Only the replies offered on this ticket: its number like #123 (or 123), or its id."),
				"limit":  integer("How many replies to return.", 1, 50, 20),
			}),
			Run: listSavedReplies,
		},
	}
}

// --- side conversations --------------------------------------------------------------

type apiThread struct {
	ID        string   `json:"id"`
	Subject   string   `json:"subject"`
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	MailboxID *string  `json:"mailbox_id"`
	Status    string   `json:"status"`
	CreatedBy *struct {
		Name string `json:"name"`
	} `json:"created_by"`
	MessageCount  int       `json:"message_count"`
	LastMessageAt time.Time `json:"last_message_at"`
	CreatedAt     time.Time `json:"created_at"`
}

type outSideConversation struct {
	ID            string    `json:"id"`
	Subject       string    `json:"subject"`
	To            []string  `json:"to"`
	Cc            []string  `json:"cc,omitempty"`
	Status        string    `json:"status"`
	StartedBy     string    `json:"started_by,omitempty"`
	MessageCount  int       `json:"message_count"`
	LastMessageAt time.Time `json:"last_message_at"`
}

func compactThread(t apiThread) outSideConversation {
	o := outSideConversation{ID: t.ID, Subject: t.Subject, To: t.To, Cc: t.Cc, Status: t.Status, MessageCount: t.MessageCount, LastMessageAt: t.LastMessageAt}
	if t.CreatedBy != nil {
		o.StartedBy = t.CreatedBy.Name
	}
	return o
}

// sideConversations lists a ticket's side conversations, oldest first.
func sideConversations(ctx context.Context, c *caller, ticketID string) ([]outSideConversation, error) {
	var list struct {
		Data []apiThread `json:"data"`
	}
	if err := c.get(ctx, "/v1/tickets/"+ticketID+"/threads", nil, &list); err != nil {
		return nil, err
	}
	var out []outSideConversation
	for _, t := range list.Data {
		out = append(out, compactThread(t))
	}
	return out, nil
}

// addresses trims, drops empties and refuses anything that is not an email address.
func addresses(in []string, field string) ([]string, error) {
	if len(in) > 20 {
		return nil, invalid("at most 20 " + field + " addresses")
	}
	var out []string
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !emailAddress(a) {
			return nil, invalid(field + " must be email addresses")
		}
		out = append(out, a)
	}
	return out, nil
}

type sideMessageResult struct {
	SideConversation outSideConversation `json:"side_conversation"`
	MessageID        string              `json:"message_id"`
	Delivery         string              `json:"delivery_status,omitempty"`
}

func startSideConversation(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket         string   `json:"ticket"`
		To             []string `json:"to"`
		Cc             []string `json:"cc"`
		Subject        string   `json:"subject"`
		Body           string   `json:"body"`
		IncludeHistory bool     `json:"include_history"`
		MailboxID      string   `json:"mailbox_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, err := ticketRef(a.Ticket)
	if err != nil {
		return nil, err
	}
	to, err := addresses(a.To, "to")
	if err != nil {
		return nil, err
	}
	if len(to) == 0 {
		return nil, invalid("to needs at least one email address")
	}
	cc, err := addresses(a.Cc, "cc")
	if err != nil {
		return nil, err
	}
	subject := strings.Join(strings.Fields(a.Subject), " ")
	if subject == "" || utf8.RuneCountInString(subject) > 500 {
		return nil, invalid("subject must be 1 to 500 characters")
	}
	html, err := markdownBody(a.Body)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"to": to, "subject": subject, "body_html": html, "include_history": a.IncludeHistory}
	if len(cc) > 0 {
		body["cc"] = cc
	}
	if a.MailboxID != "" {
		id, err := idRef(a.MailboxID, "mailbox_id")
		if err != nil {
			return nil, err
		}
		body["mailbox_id"] = id
	}
	var created struct {
		Thread  apiThread `json:"thread"`
		Message struct {
			ID             string `json:"id"`
			DeliveryStatus string `json:"delivery_status"`
		} `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/tickets/"+ref+"/threads", nil, body, &created); err != nil {
		return nil, err
	}
	out := sideMessageResult{SideConversation: compactThread(created.Thread), MessageID: created.Message.ID}
	if created.Message.DeliveryStatus != "none" {
		out.Delivery = created.Message.DeliveryStatus
	}
	return out, nil
}

func replyToSideConversation(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket           string `json:"ticket"`
		SideConversation string `json:"side_conversation"`
		Body             string `json:"body"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, err := ticketRef(a.Ticket)
	if err != nil {
		return nil, err
	}
	th, err := idRef(a.SideConversation, "side_conversation")
	if err != nil {
		return nil, err
	}
	html, err := markdownBody(a.Body)
	if err != nil {
		return nil, err
	}
	var m struct {
		ID             string `json:"id"`
		TicketID       string `json:"ticket_id"`
		DeliveryStatus string `json:"delivery_status"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/tickets/"+ref+"/threads/"+th+"/messages", nil, map[string]any{"body_html": html}, &m); err != nil {
		return nil, err
	}
	out := struct {
		MessageID          string `json:"message_id"`
		TicketID           string `json:"ticket_id"`
		SideConversationID string `json:"side_conversation_id"`
		Delivery           string `json:"delivery_status,omitempty"`
	}{MessageID: m.ID, TicketID: m.TicketID, SideConversationID: th}
	if m.DeliveryStatus != "none" {
		out.Delivery = m.DeliveryStatus
	}
	return out, nil
}

func updateSideConversation(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket           string `json:"ticket"`
		SideConversation string `json:"side_conversation"`
		Status           string `json:"status"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, err := ticketRef(a.Ticket)
	if err != nil {
		return nil, err
	}
	th, err := idRef(a.SideConversation, "side_conversation")
	if err != nil {
		return nil, err
	}
	if a.Status == "" {
		return nil, invalid("status must be open or closed")
	}
	if err := oneOf(a.Status, "status", "open", "closed"); err != nil {
		return nil, err
	}
	var t apiThread
	if err := c.do(ctx, http.MethodPatch, "/v1/tickets/"+ref+"/threads/"+th, nil, map[string]any{"status": a.Status}, &t); err != nil {
		return nil, err
	}
	return compactThread(t), nil
}

// --- merging -------------------------------------------------------------------------

func mergeTickets(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket string `json:"ticket"`
		Into   string `json:"into"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, err := ticketRef(a.Ticket)
	if err != nil {
		return nil, err
	}
	into, err := ticketRef(a.Into)
	if err != nil {
		return nil, invalid("into must be a ticket number like #123 or a ticket id")
	}
	if ref == into {
		return nil, invalid("a ticket cannot be merged into itself")
	}
	// The body carries the reference as written, not path-escaped.
	intoRef, _ := url.PathUnescape(into)
	var t apiTicket
	if err := c.do(ctx, http.MethodPost, "/v1/tickets/"+ref+"/merge", nil, map[string]any{"into_id": intoRef}, &t); err != nil {
		return nil, err
	}
	o := compactTicket(t, false)
	c.name(ctx, &o)
	return struct {
		Merged string    `json:"merged"`
		Into   outTicket `json:"into"`
	}{Merged: a.Ticket, Into: o}, nil
}

// --- notes ---------------------------------------------------------------------------

// noteRef validates a ticket and message reference for the note tools.
func noteRef(ticket, messageID string) (string, string, error) {
	ref, err := ticketRef(ticket)
	if err != nil {
		return "", "", err
	}
	mid, err := idRef(messageID, "message_id")
	if err != nil {
		return "", "", err
	}
	return ref, mid, nil
}

func editNote(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket    string `json:"ticket"`
		MessageID string `json:"message_id"`
		Body      string `json:"body"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, mid, err := noteRef(a.Ticket, a.MessageID)
	if err != nil {
		return nil, err
	}
	html, err := markdownBody(a.Body)
	if err != nil {
		return nil, err
	}
	// The API edits internal notes only, the caller's own, never a side conversation's.
	var m struct {
		ID       string `json:"id"`
		TicketID string `json:"ticket_id"`
		BodyText string `json:"body_text"`
	}
	if err := c.do(ctx, http.MethodPatch, "/v1/tickets/"+ref+"/messages/"+mid, nil, map[string]any{"body_html": html}, &m); err != nil {
		return nil, err
	}
	return struct {
		MessageID string `json:"message_id"`
		TicketID  string `json:"ticket_id"`
		Text      string `json:"text"`
	}{MessageID: m.ID, TicketID: m.TicketID, Text: clip(m.BodyText, messageChars)}, nil
}

func deleteNote(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket    string `json:"ticket"`
		MessageID string `json:"message_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, mid, err := noteRef(a.Ticket, a.MessageID)
	if err != nil {
		return nil, err
	}
	// The API deletes replies too, where the workspace allows it; this tool deletes notes
	// only, so it looks the message up first.
	var t apiTicket
	if err := c.get(ctx, "/v1/tickets/"+ref, nil, &t); err != nil {
		return nil, err
	}
	var msgs struct {
		Data []apiMessage `json:"data"`
	}
	if err := c.get(ctx, "/v1/tickets/"+t.ID+"/messages", nil, &msgs); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(msgs.Data, func(m apiMessage) bool { return m.ID == mid })
	if i < 0 {
		return nil, invalid("message_id is not a message of this ticket")
	}
	if m := msgs.Data[i]; m.DeletedAt == nil {
		if m.Kind != "note" {
			return nil, invalid("message_id is not an internal note; delete_note deletes notes only")
		}
		// Deleting an already deleted note succeeds, as the API's delete does.
		if err := c.do(ctx, http.MethodDelete, "/v1/tickets/"+t.ID+"/messages/"+mid, nil, nil, nil); err != nil {
			return nil, err
		}
	}
	return struct {
		MessageID string `json:"message_id"`
		TicketID  string `json:"ticket_id"`
		Deleted   bool   `json:"deleted"`
		Note      string `json:"note"`
	}{MessageID: mid, TicketID: t.ID, Deleted: true, Note: "a marker stays in the conversation; an admin can restore the note in the app"}, nil
}

// --- activity ------------------------------------------------------------------------

const (
	maxActivities = 200
	// activityChars bounds each detail of an activity.
	activityChars = 300
)

type apiActivity struct {
	Type  string `json:"type"`
	Actor struct {
		Type string   `json:"type"`
		User *userRef `json:"user"`
	} `json:"actor"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}

type outActivity struct {
	Type string         `json:"type"`
	By   string         `json:"by"`
	Name string         `json:"name,omitempty"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

func getTicketActivity(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Ticket string `json:"ticket"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	ref, err := ticketRef(a.Ticket)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, maxActivities)
	var list struct {
		Data []apiActivity `json:"data"`
	}
	if err := c.get(ctx, "/v1/tickets/"+ref+"/activities", nil, &list); err != nil {
		return nil, err
	}
	out := struct {
		Activities     []outActivity `json:"activities"`
		EarlierOmitted int           `json:"earlier_omitted,omitempty"`
	}{Activities: []outActivity{}}
	acts := list.Data
	if len(acts) > limit {
		out.EarlierOmitted = len(acts) - limit
		acts = acts[len(acts)-limit:]
	}
	for _, x := range acts {
		o := outActivity{Type: x.Type, By: x.Actor.Type, At: x.CreatedAt, Data: activityData(x.Data)}
		if x.Actor.User != nil {
			o.Name = x.Actor.User.Name
		}
		out.Activities = append(out.Activities, o)
	}
	return out, nil
}

// activityData keeps an activity's details small: short text and plain values as they
// are, long text clipped, and nested lists and objects only when they are small.
func activityData(d map[string]any) map[string]any {
	if len(d) == 0 {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		switch x := v.(type) {
		case string:
			out[k] = clip(x, activityChars)
		case nil, bool, float64, int, int64:
			out[k] = x
		default:
			if b, err := json.Marshal(x); err == nil && len(b) <= activityChars {
				out[k] = x
			}
		}
	}
	return out
}

// --- saved replies -------------------------------------------------------------------

func listSavedReplies(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Query  string `json:"query"`
		Ticket string `json:"ticket"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{"limit": {strconv.Itoa(min(limit, 50))}}
	if s := strings.TrimSpace(a.Query); s != "" {
		q.Set("q", searchText(s))
	}
	if strings.TrimSpace(a.Ticket) != "" {
		ref, err := ticketRef(a.Ticket)
		if err != nil {
			return nil, err
		}
		var t apiTicket
		if err := c.get(ctx, "/v1/tickets/"+ref, nil, &t); err != nil {
			return nil, err
		}
		q.Set("ticket_id", t.ID)
	}
	var pg page[struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Shortcut string  `json:"shortcut"`
		BodyHTML string  `json:"body_html"`
		TeamID   *string `json:"team_id"`
	}]
	if err := c.get(ctx, "/v1/saved-replies", q, &pg); err != nil {
		return nil, err
	}
	type reply struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Shortcut string  `json:"shortcut,omitempty"`
		TeamID   *string `json:"team_id,omitempty"`
		Text     string  `json:"text"`
	}
	out := struct {
		SavedReplies []reply `json:"saved_replies"`
		Count        int     `json:"count"`
		More         bool    `json:"more,omitempty"`
	}{SavedReplies: []reply{}, More: pg.NextCursor != nil}
	for _, r := range pg.Data {
		out.SavedReplies = append(out.SavedReplies, reply{ID: r.ID, Title: r.Title, Shortcut: r.Shortcut, TeamID: r.TeamID,
			Text: clip(hd.HTMLToText(r.BodyHTML), messageChars)})
	}
	out.Count = len(out.SavedReplies)
	return out, nil
}
