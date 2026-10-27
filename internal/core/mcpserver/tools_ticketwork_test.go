package mcpserver

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/ids"
)

// mailbox adds a forwarding mailbox of the workspace's brand that sends through the system.
func (e *env) mailbox() uuid.UUID {
	e.T.Helper()
	id := ids.New()
	e.Exec(`INSERT INTO mailboxes (id, workspace_id, name, email, display_name, inbound_kind, forwarding_token, outbound_kind, brand_id)
		VALUES ($1, $2, 'Support', $3, 'Acme Support', 'forwarding', $4, 'system', $5)`, id, e.WS.ID, "support-"+id.String()[24:]+"@acme.test", "fw-"+id.String(), e.WS.BrandID)
	return id
}

func TestUpdateTicketMoreFields(t *testing.T) {
	e := newEnv(t)
	tk := e.ticket("Invoice question", "Where is my invoice?")
	s := e.session(e.APIKey(e.WS, auth.ScopeTicketsRead, auth.ScopeTicketsWrite, auth.ScopeContactsRead, auth.ScopeContactsWrite))

	var contact struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	call(t, s, "create_contact", map[string]any{"email": "billing@customer.test", "name": "Billing Team", "notes": "Pays by invoice"}, &contact)
	if contact.ID == "" || contact.Email != "billing@customer.test" {
		t.Fatalf("create_contact: %+v", contact)
	}

	// The contact moves by email; the subject, snooze and spam flag change in the same call.
	until := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	var upd struct {
		Subject string `json:"subject"`
		Contact struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"contact"`
		SnoozedUntil *time.Time `json:"snoozed_until"`
	}
	call(t, s, "update_ticket", map[string]any{"ticket": fmt.Sprintf("#%d", tk.Number), "contact": "Billing@Customer.test",
		"subject": "  Invoice   for March ", "snooze_until": until}, &upd)
	if upd.Contact.ID != contact.ID || upd.Subject != "Invoice for March" || upd.SnoozedUntil == nil {
		t.Fatalf("update_ticket: %+v", upd)
	}
	var contactID uuid.UUID
	e.Scalar(`SELECT contact_id FROM tickets WHERE id = $1`, &contactID, tk.ID)
	if contactID.String() != contact.ID {
		t.Fatalf("stored contact %s", contactID)
	}

	// "none" wakes it; spam takes it out of the inbox, and back.
	call(t, s, "update_ticket", map[string]any{"ticket": tk.ID.String(), "snooze_until": "none", "spam": true}, &upd)
	var snoozed *time.Time
	var spam bool
	e.Scalar(`SELECT snoozed_until FROM tickets WHERE id = $1`, &snoozed, tk.ID)
	e.Scalar(`SELECT spam FROM tickets WHERE id = $1`, &spam, tk.ID)
	if snoozed != nil || !spam {
		t.Fatalf("snoozed %v spam %v", snoozed, spam)
	}
	call(t, s, "update_ticket", map[string]any{"ticket": tk.ID.String(), "spam": false}, &upd)

	// A contact that does not exist, and a bad snooze time, are refused before any change.
	if res := call(t, s, "update_ticket", map[string]any{"ticket": tk.ID.String(), "contact": "nobody@nowhere.test"}, nil); !res.IsError {
		t.Fatal("unknown contact accepted")
	}
	// A snooze needs a time with its offset: a bare day has no time zone to wake in.
	for _, bad := range []string{"tomorrow", "2030-01-02"} {
		if res := call(t, s, "update_ticket", map[string]any{"ticket": tk.ID.String(), "snooze_until": bad}, nil); !res.IsError {
			t.Fatalf("snooze_until %q accepted", bad)
		}
	}

	// update_contact finds the contact by email and changes only what it is given.
	var changed struct {
		Name   string   `json:"name"`
		Phone  string   `json:"phone"`
		Notes  string   `json:"notes"`
		Labels []string `json:"labels"`
	}
	call(t, s, "update_contact", map[string]any{"contact": "billing@customer.test", "phone": "+41 44 000 00 00", "add_labels": []string{"vip"}}, &changed)
	if changed.Name != "Billing Team" || changed.Phone != "+41 44 000 00 00" || changed.Notes != "Pays by invoice" || !slices.Contains(changed.Labels, "vip") {
		t.Fatalf("update_contact: %+v", changed)
	}
	// An empty phone clears it; an empty name is refused.
	call(t, s, "update_contact", map[string]any{"contact": contact.ID, "phone": ""}, &struct{}{})
	var phone, name string
	e.Scalar(`SELECT phone FROM contacts WHERE id = $1`, &phone, contact.ID)
	e.Scalar(`SELECT name FROM contacts WHERE id = $1`, &name, contact.ID)
	if phone != "" || name != "Billing Team" {
		t.Fatalf("phone not cleared: %q %q", phone, name)
	}
	if res := call(t, s, "update_contact", map[string]any{"contact": contact.ID, "name": " "}, nil); !res.IsError {
		t.Fatal("empty name accepted")
	}

	// Without contacts:read the email lookup is explained; the id still works.
	w := e.session(e.APIKey(e.WS, auth.ScopeTicketsRead, auth.ScopeTicketsWrite))
	if res := call(t, w, "update_ticket", map[string]any{"ticket": tk.ID.String(), "contact": "billing@customer.test"}, nil); !res.IsError ||
		!strings.Contains(text(res), "contacts:read") {
		t.Fatalf("lookup without contacts:read: %s", text(res))
	}
	call(t, w, "update_ticket", map[string]any{"ticket": tk.ID.String(), "contact": contact.ID}, &upd)
}

func TestSideConversationsThroughMCP(t *testing.T) {
	e := newEnv(t)
	mb := e.mailbox()
	s := e.session(e.APIKey(e.WS, auth.ScopeTicketsRead, auth.ScopeTicketsWrite))
	var tk struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
	}
	call(t, s, "create_ticket", map[string]any{"email": "carla@customer.test", "subject": "Missing payout", "body": "Payout for order 1182 is missing.",
		"kind": "note", "mailbox_id": mb.String()}, &tk)
	ref := fmt.Sprintf("#%d", tk.Number)

	// Recipients are required and must be addresses.
	if res := call(t, s, "start_side_conversation", map[string]any{"ticket": ref, "to": []string{}, "subject": "Payout", "body": "Hi"}, nil); !res.IsError {
		t.Fatal("no recipients accepted")
	}
	if res := call(t, s, "start_side_conversation", map[string]any{"ticket": ref, "to": []string{"ops"}, "subject": "Payout", "body": "Hi"}, nil); !res.IsError {
		t.Fatal("a recipient without @ accepted")
	}

	var started struct {
		SideConversation struct {
			ID      string   `json:"id"`
			Subject string   `json:"subject"`
			To      []string `json:"to"`
			Status  string   `json:"status"`
		} `json:"side_conversation"`
		MessageID string `json:"message_id"`
		Delivery  string `json:"delivery_status"`
	}
	call(t, s, "start_side_conversation", map[string]any{"ticket": ref, "to": []string{"ops@acquirer.test"}, "subject": "Payout for order 1182",
		"body": "Can you check whether the **payout** went out?"}, &started)
	sc := started.SideConversation
	if sc.ID == "" || sc.Status != "open" || sc.Subject != "Payout for order 1182" || !slices.Equal(sc.To, []string{"ops@acquirer.test"}) ||
		started.MessageID == "" || started.Delivery != "pending" {
		t.Fatalf("start_side_conversation: %+v", started)
	}
	var html string
	e.Scalar(`SELECT body_html FROM messages WHERE id = $1 AND thread_id = $2 AND kind = 'note'`, &html, started.MessageID, sc.ID)
	if !strings.Contains(html, "<strong>payout</strong>") {
		t.Fatalf("first message %q", html)
	}

	// get_ticket lists it, and its message carries the side conversation.
	var read struct {
		SideConversations []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"side_conversations"`
		Messages []struct {
			ID                 string `json:"id"`
			SideConversationID string `json:"side_conversation_id"`
		} `json:"messages"`
	}
	call(t, s, "get_ticket", map[string]any{"ticket": ref}, &read)
	if len(read.SideConversations) != 1 || read.SideConversations[0].ID != sc.ID {
		t.Fatalf("side conversations in get_ticket: %+v", read.SideConversations)
	}
	marked := false
	for _, m := range read.Messages {
		marked = marked || (m.ID == started.MessageID && m.SideConversationID == sc.ID)
	}
	if !marked {
		t.Fatalf("messages in get_ticket: %+v", read.Messages)
	}

	// Closing it, writing again (which reopens it), and a side conversation of another ticket.
	var closed struct {
		Status string `json:"status"`
	}
	call(t, s, "update_side_conversation", map[string]any{"ticket": ref, "side_conversation": sc.ID, "status": "closed"}, &closed)
	if closed.Status != "closed" {
		t.Fatalf("closed: %+v", closed)
	}
	var next struct {
		MessageID string `json:"message_id"`
	}
	call(t, s, "reply_to_side_conversation", map[string]any{"ticket": ref, "side_conversation": sc.ID, "body": "Any news?"}, &next)
	var status string
	var count int
	e.Scalar(`SELECT status FROM ticket_threads WHERE id = $1`, &status, sc.ID)
	e.Scalar(`SELECT count(*) FROM messages WHERE thread_id = $1`, &count, sc.ID)
	if next.MessageID == "" || status != "open" || count != 2 {
		t.Fatalf("reply: %+v status %s count %d", next, status, count)
	}
	other := e.ticket("Other", "Unrelated")
	if res := call(t, s, "reply_to_side_conversation", map[string]any{"ticket": other.ID.String(), "side_conversation": sc.ID, "body": "x"}, nil); !res.IsError {
		t.Fatal("a side conversation answered through another ticket")
	}
}

func TestMergeActivityAndSavedReplies(t *testing.T) {
	e := newEnv(t)
	keep := e.ticket("Refund for order 77", "Please refund order 77.")
	dup := e.ticket("Refund again", "Still waiting on the refund for order 77.")
	s := e.session(e.APIKey(e.WS, auth.ScopeTicketsRead, auth.ScopeTicketsWrite))

	if res := call(t, s, "merge_tickets", map[string]any{"ticket": keep.ID.String(), "into": keep.ID.String()}, nil); !res.IsError {
		t.Fatal("merged into itself")
	}
	var merged struct {
		Into struct {
			ID           string `json:"id"`
			MessageCount int    `json:"message_count"`
		} `json:"into"`
	}
	call(t, s, "merge_tickets", map[string]any{"ticket": fmt.Sprintf("#%d", dup.Number), "into": fmt.Sprintf("#%d", keep.Number)}, &merged)
	var dupStatus string
	e.Scalar(`SELECT status FROM tickets WHERE id = $1`, &dupStatus, dup.ID)
	if merged.Into.ID != keep.ID.String() || merged.Into.MessageCount != 2 || dupStatus != "closed" {
		t.Fatalf("merge: %+v duplicate %s", merged, dupStatus)
	}

	call(t, s, "update_ticket", map[string]any{"ticket": keep.ID.String(), "priority": "urgent"}, &struct{}{})
	var acts struct {
		Activities []struct {
			Type string `json:"type"`
			By   string `json:"by"`
		} `json:"activities"`
		EarlierOmitted int `json:"earlier_omitted"`
	}
	call(t, s, "get_ticket_activity", map[string]any{"ticket": fmt.Sprintf("#%d", keep.Number)}, &acts)
	if len(acts.Activities) == 0 || acts.Activities[len(acts.Activities)-1].By != "api" {
		t.Fatalf("activity: %+v", acts)
	}
	call(t, s, "get_ticket_activity", map[string]any{"ticket": keep.ID.String(), "limit": 1}, &acts)
	if len(acts.Activities) != 1 || acts.EarlierOmitted == 0 {
		t.Fatalf("limited activity: %+v", acts)
	}

	e.Exec(`INSERT INTO saved_replies (id, workspace_id, title, shortcut, body_html) VALUES ($1, $2, 'Refund sent', 'refund', '<p>Your refund is <b>on its way</b>.</p>')`,
		ids.New(), e.WS.ID)
	e.Exec(`INSERT INTO saved_replies (id, workspace_id, title, shortcut, body_html) VALUES ($1, $2, 'Password reset', 'reset', '<p>Use the reset link.</p>')`,
		ids.New(), e.WS.ID)
	var replies struct {
		SavedReplies []struct {
			Title    string `json:"title"`
			Shortcut string `json:"shortcut"`
			Text     string `json:"text"`
		} `json:"saved_replies"`
		Count int `json:"count"`
	}
	call(t, s, "list_saved_replies", map[string]any{"query": "refund", "ticket": keep.ID.String()}, &replies)
	if replies.Count != 1 || replies.SavedReplies[0].Shortcut != "refund" || replies.SavedReplies[0].Text != "Your refund is on its way." {
		t.Fatalf("saved replies: %+v", replies)
	}
}

func TestNotesChangeOnlyAsAPerson(t *testing.T) {
	e := newEnv(t)
	tk := e.ticket("Login issue", "I can't sign in.")

	// An API key is not a teammate: it is never offered the note tools.
	key := toolNames(t, e.session(e.APIKey(e.WS, auth.AllScopes...)))
	if slices.Contains(key, "edit_note") || slices.Contains(key, "delete_note") {
		t.Fatalf("api key tools: %v", key)
	}

	_, at := e.connectAs(e.WS.Agent, "")
	s := e.session(at.AccessToken)
	if got := toolNames(t, s); !slices.Contains(got, "edit_note") || !slices.Contains(got, "delete_note") {
		t.Fatalf("agent tools: %v", got)
	}
	var note struct {
		MessageID string `json:"message_id"`
	}
	call(t, s, "add_note", map[string]any{"ticket": tk.ID.String(), "body": "Asked for a screenshot"}, &note)
	var edited struct {
		Text string `json:"text"`
	}
	call(t, s, "edit_note", map[string]any{"ticket": tk.ID.String(), "message_id": note.MessageID, "body": "Asked for a **screenshot** of the error"}, &edited)
	if edited.Text != "Asked for a screenshot of the error" {
		t.Fatalf("edit_note: %+v", edited)
	}

	// The customer's message is not a note; another agent's note is theirs to change.
	var first string
	e.Scalar(`SELECT id FROM messages WHERE ticket_id = $1 AND kind = 'reply'`, &first, tk.ID)
	if res := call(t, s, "edit_note", map[string]any{"ticket": tk.ID.String(), "message_id": first, "body": "x"}, nil); !res.IsError {
		t.Fatal("a reply was edited")
	}
	if res := call(t, s, "delete_note", map[string]any{"ticket": tk.ID.String(), "message_id": first}, nil); !res.IsError {
		t.Fatal("a reply was deleted")
	}

	call(t, s, "delete_note", map[string]any{"ticket": tk.ID.String(), "message_id": note.MessageID}, &struct{}{})
	var deleted bool
	e.Scalar(`SELECT deleted_at IS NOT NULL FROM messages WHERE id = $1`, &deleted, note.MessageID)
	if !deleted {
		t.Fatal("note not deleted")
	}
	// Deleting it again succeeds without a change, as a retry would.
	var again struct {
		Deleted bool `json:"deleted"`
	}
	call(t, s, "delete_note", map[string]any{"ticket": tk.ID.String(), "message_id": note.MessageID}, &again)
	if !again.Deleted {
		t.Fatalf("second delete: %+v", again)
	}
	var noteDeletes int
	e.Scalar(`SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'note_deleted'`, &noteDeletes, tk.ID)
	if noteDeletes != 1 {
		t.Fatalf("%d note_deleted activities", noteDeletes)
	}

	// Settings > AI clients says which tools need a sign-in.
	var server AIClientServer
	e.Do("GET", "/v1/ai-clients/server", e.WS.Agent.As(e.WS), nil).Want(t, 200).JSON(t, &server)
	for _, tl := range server.Tools {
		if want := tl.Name == "edit_note" || tl.Name == "delete_note"; tl.PeopleOnly != want {
			t.Fatalf("%s people_only = %v", tl.Name, tl.PeopleOnly)
		}
	}
}
