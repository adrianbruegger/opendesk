package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/useopendesk/opendesk/internal/ai"
	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
)

// --- contacts ------------------------------------------------------------------------

func contactTools() []*tool {
	return []*tool{
		{
			Name: "search_contacts", Title: "Search contacts", Scopes: []string{auth.ScopeContactsRead}, ReadOnly: true, Idempotent: true,
			Description: "Find customers (contacts) by name, email, phone or custom field value. Without a query, lists the newest contacts.",
			Schema: obj(map[string]any{
				"query": strN("What to search for: a name, an email or part of one, a phone number or a field value.", 1, 200),
				"limit": integer("How many contacts to return.", 1, 50, 10),
			}),
			Run: searchContacts,
		},
		{
			Name: "get_contact", Title: "Get a contact", Scopes: []string{auth.ScopeContactsRead}, ReadOnly: true, Idempotent: true,
			Description: "Read one contact by id or email: details, company, labels, custom fields and notes, plus their latest tickets when the key can read tickets.",
			Schema:      obj(map[string]any{"contact": str("The contact's id, or their exact email address.")}, "contact"),
			Run:         getContact,
		},
		{
			Name: "create_contact", Title: "Create a contact", Scopes: []string{auth.ScopeContactsWrite},
			Description: "Add a customer (contact) with an email address, a name or both. An email that already belongs to a contact is refused. " +
				"Returns the new contact.",
			Schema: obj(contactWriteProps(false)),
			Run:    createContact,
		},
		{
			Name: "update_contact", Title: "Update a contact", Scopes: []string{auth.ScopeContactsWrite}, Destructive: true, Idempotent: true,
			Description: "Change a contact's name, email, phone, notes, labels or custom fields. Only the fields you pass change. Returns the updated contact.",
			Schema:      obj(contactWriteProps(true), "contact"),
			Run:         updateContact,
		},
	}
}

func contactWriteProps(update bool) map[string]any {
	props := map[string]any{
		"email":         strN("Email address.", 3, 254),
		"name":          strN("Full name.", 1, 200),
		"phone":         strN("Phone number.", 1, 50),
		"notes":         strN("Notes about the contact, for the team.", 0, 10000),
		"add_labels":    arr("Label names to add.", strN("", 1, 50), 50),
		"remove_labels": arr("Label names to remove.", strN("", 1, 50), 50),
		"fields":        object("Custom field values by key, merged; null clears a field."),
	}
	if update {
		props["contact"] = str("The contact: their id, or their exact email address.")
		props["notes"] = strN("Notes about the contact, for the team. Replaces the notes; empty clears them.", 0, 10000)
	}
	return props
}

type contactWriteArgs struct {
	Contact      string         `json:"contact"`
	Email        string         `json:"email"`
	Name         string         `json:"name"`
	Phone        string         `json:"phone"`
	Notes        *string        `json:"notes"`
	AddLabels    []string       `json:"add_labels"`
	RemoveLabels []string       `json:"remove_labels"`
	Fields       map[string]any `json:"fields"`
}

// body turns the arguments into a contact write body for the API.
func (a contactWriteArgs) body() (map[string]any, error) {
	body := map[string]any{}
	if v := strings.TrimSpace(a.Email); v != "" {
		if !strings.Contains(v, "@") || utf8.RuneCountInString(v) > 254 {
			return nil, invalid("email must be an email address")
		}
		body["email"] = v
	}
	if v := strings.TrimSpace(a.Name); v != "" {
		if utf8.RuneCountInString(v) > 200 {
			return nil, invalid("name is at most 200 characters")
		}
		body["name"] = v
	}
	if v := strings.TrimSpace(a.Phone); v != "" {
		if utf8.RuneCountInString(v) > 50 {
			return nil, invalid("phone is at most 50 characters")
		}
		body["phone"] = v
	}
	if a.Notes != nil {
		if utf8.RuneCountInString(*a.Notes) > 10000 {
			return nil, invalid("notes are at most 10,000 characters")
		}
		body["notes"] = strings.TrimSpace(*a.Notes)
	}
	add, err := cleanLabels(a.AddLabels)
	if err != nil {
		return nil, err
	}
	remove, err := cleanLabels(a.RemoveLabels)
	if err != nil {
		return nil, err
	}
	if len(add) > 0 {
		body["add_tags"] = add
	}
	if len(remove) > 0 {
		body["remove_tags"] = remove
	}
	if len(a.Fields) > 0 {
		body["fields"] = a.Fields
	}
	return body, nil
}

func createContact(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[contactWriteArgs](raw)
	if err != nil {
		return nil, err
	}
	if a.Contact != "" {
		return nil, invalid("contact is for update_contact; create_contact takes email and name")
	}
	body, err := a.body()
	if err != nil {
		return nil, err
	}
	if body["email"] == nil && body["name"] == nil {
		return nil, invalid("a contact needs an email or a name")
	}
	var ct apiContact
	if err := c.do(ctx, http.MethodPost, "/v1/contacts", nil, body, &ct); err != nil {
		return nil, err
	}
	return compactContact(ct, true), nil
}

func updateContact(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[contactWriteArgs](raw)
	if err != nil {
		return nil, err
	}
	id, err := resolveContact(ctx, c, a.Contact)
	if err != nil {
		return nil, err
	}
	body, err := a.body()
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, invalid("nothing to change: pass email, name, phone, notes, add_labels, remove_labels or fields")
	}
	var ct apiContact
	if err := c.do(ctx, http.MethodPatch, "/v1/contacts/"+id, nil, body, &ct); err != nil {
		return nil, err
	}
	return compactContact(ct, true), nil
}

type apiContact struct {
	ID      string  `json:"id"`
	Email   *string `json:"email"`
	Name    string  `json:"name"`
	Phone   string  `json:"phone"`
	Company *struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Domain string `json:"domain"`
	} `json:"company"`
	ExternalID  *string        `json:"external_id"`
	Notes       string         `json:"notes"`
	Tags        []tagRef       `json:"tags"`
	Fields      map[string]any `json:"fields"`
	Blocked     bool           `json:"blocked"`
	TicketCount int            `json:"ticket_count"`
	CreatedAt   time.Time      `json:"created_at"`
}

type outCompany struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain,omitempty"`
}

type outContact struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Email       string         `json:"email,omitempty"`
	Phone       string         `json:"phone,omitempty"`
	Company     *outCompany    `json:"company,omitempty"`
	ExternalID  string         `json:"external_id,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Fields      map[string]any `json:"fields,omitempty"`
	Notes       string         `json:"notes,omitempty"`
	Blocked     bool           `json:"blocked,omitempty"`
	TicketCount int            `json:"ticket_count"`
	CreatedAt   time.Time      `json:"created_at"`
}

func compactContact(c apiContact, full bool) outContact {
	o := outContact{ID: c.ID, Name: c.Name, Phone: c.Phone, Blocked: c.Blocked, TicketCount: c.TicketCount, CreatedAt: c.CreatedAt}
	if c.Email != nil {
		o.Email = *c.Email
	}
	if c.Company != nil {
		o.Company = &outCompany{ID: c.Company.ID, Name: c.Company.Name, Domain: c.Company.Domain}
	}
	for _, t := range c.Tags {
		o.Labels = append(o.Labels, t.Name)
	}
	if full {
		if c.ExternalID != nil {
			o.ExternalID = *c.ExternalID
		}
		if len(c.Fields) > 0 {
			o.Fields = c.Fields
		}
		o.Notes = clip(c.Notes, 2000)
	}
	return o
}

func searchContacts(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if s := strings.TrimSpace(a.Query); s != "" {
		q.Set("q", clip(s, 200))
	}
	var pg page[apiContact]
	if err := c.get(ctx, "/v1/contacts", q, &pg); err != nil {
		return nil, err
	}
	out := struct {
		Contacts []outContact `json:"contacts"`
		Count    int          `json:"count"`
		More     bool         `json:"more,omitempty"`
	}{Contacts: []outContact{}, More: pg.NextCursor != nil}
	for _, ct := range pg.Data {
		out.Contacts = append(out.Contacts, compactContact(ct, false))
	}
	out.Count = len(out.Contacts)
	return out, nil
}

func getContact(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Contact string `json:"contact"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := resolveContact(ctx, c, a.Contact)
	if err != nil {
		return nil, err
	}
	var ct apiContact
	if err := c.get(ctx, "/v1/contacts/"+id, nil, &ct); err != nil {
		return nil, err
	}
	out := struct {
		Contact outContact  `json:"contact"`
		Tickets []outTicket `json:"recent_tickets,omitempty"`
	}{Contact: compactContact(ct, true)}
	if c.p.HasScope(auth.ScopeTicketsRead) {
		var pg page[apiTicket]
		if err := c.get(ctx, "/v1/contacts/"+id+"/tickets", url.Values{"limit": {"10"}}, &pg); err == nil {
			for _, t := range pg.Data {
				out.Tickets = append(out.Tickets, compactTicket(t, false))
			}
			c.nameAll(ctx, out.Tickets)
		}
	}
	return out, nil
}

// resolveContact turns a contact id or exact email address into a contact id. An email
// is looked up through the contacts list, which needs the contacts:read scope.
func resolveContact(ctx context.Context, c *caller, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !strings.Contains(v, "@") {
		id, err := idRef(v, "contact")
		if err != nil {
			return "", invalid("contact must be an id or an email address")
		}
		return id, nil
	}
	if utf8.RuneCountInString(v) > 254 {
		return "", invalid("contact must be an id or an email address")
	}
	var pg page[apiContact]
	if err := c.get(ctx, "/v1/contacts", url.Values{"q": {v}, "limit": {"20"}}, &pg); err != nil {
		if statusOf(err) == http.StatusForbidden {
			return "", invalid("looking up a contact by email needs the contacts:read scope; pass the contact id instead")
		}
		return "", err
	}
	for _, ct := range pg.Data {
		if ct.Email != nil && strings.EqualFold(*ct.Email, v) {
			return ct.ID, nil
		}
	}
	return "", invalid("no contact has the email " + strconv.Quote(v))
}

// --- knowledge -----------------------------------------------------------------------

// Passage is a piece of a knowledge source (website page, file, snippet) that matched a
// search (contracts/agent.md, "Knowledge sources").
type Passage struct {
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	URL      string `json:"url,omitempty"`
	Text     string `json:"text"`
}

// SearchSources finds passages in the workspace's knowledge sources for search_knowledge.
// It is called with the API key's principal (workspace and scopes) and must scope its
// reads to p.WorkspaceID. Until it is set, search_knowledge answers from KB articles only.
var SearchSources func(ctx context.Context, p *auth.Principal, query string, limit int) ([]Passage, error)

func knowledgeTools() []*tool {
	return []*tool{
		{
			Name: "search_knowledge", Title: "Search the knowledge base", Scopes: []string{auth.ScopeKBRead}, ReadOnly: true, Idempotent: true,
			Description: "Search the help center's knowledge base articles and the workspace's other knowledge sources by text. " +
				"Returns the best matches, the top ones with their full text, to answer questions or cite in replies.",
			Schema: obj(map[string]any{
				"query":  strN("What to search for.", 1, 200),
				"status": enum("Which articles: published (the default), draft or any.", "published", "draft", "any"),
				"limit":  integer("How many articles to return.", 1, 10, 5),
			}, "query"),
			Run: searchKnowledge,
		},
		{
			Name: "create_kb_draft", Title: "Create a draft article", Scopes: []string{auth.ScopeKBWrite},
			Description: "Create a knowledge base article as an unpublished draft for the team to review and publish. Markdown body.",
			Schema: obj(map[string]any{
				"title":         strN("The article title.", 1, 200),
				"body":          strN("The article, in markdown.", 1, 50000),
				"collection_id": str("Put the draft in this collection (optional)."),
			}, "title", "body"),
			Run: createKBDraft,
		},
	}
}

// Full text is returned for the top fullTextArticles matches.
const (
	fullTextArticles = 3
	articleChars     = 6000
)

type apiArticle struct {
	ID           string     `json:"id"`
	CollectionID *string    `json:"collection_id"`
	Title        string     `json:"title"`
	Slug         string     `json:"slug"`
	Excerpt      string     `json:"excerpt"`
	BodyHTML     string     `json:"body_html"`
	Status       string     `json:"status"`
	PublishedAt  *time.Time `json:"published_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type outArticle struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Slug         string    `json:"slug"`
	Status       string    `json:"status"`
	CollectionID *string   `json:"collection_id,omitempty"`
	Excerpt      string    `json:"excerpt,omitempty"`
	Text         string    `json:"text,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func searchKnowledge(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Query  string `json:"query"`
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return nil, invalid("query is empty")
	}
	query = clip(query, 200)
	if err := oneOf(a.Status, "status", "published", "draft", "any"); err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 5
	}
	limit = min(limit, 10)
	q := url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}}
	switch a.Status {
	case "", "published":
		q.Set("status", "published")
	case "draft":
		q.Set("status", "draft")
	}
	var pg page[apiArticle]
	if err := c.get(ctx, "/v1/kb/articles", q, &pg); err != nil {
		return nil, err
	}
	out := struct {
		Articles []outArticle `json:"articles"`
		Sources  []Passage    `json:"sources,omitempty"`
	}{Articles: []outArticle{}}
	for i, ar := range pg.Data {
		o := outArticle{ID: ar.ID, Title: ar.Title, Slug: ar.Slug, Status: ar.Status, CollectionID: ar.CollectionID, UpdatedAt: ar.UpdatedAt}
		if i < fullTextArticles {
			o.Text = clip(hd.HTMLToText(ar.BodyHTML), articleChars)
		} else {
			o.Excerpt = clip(ar.Excerpt, 300)
		}
		out.Articles = append(out.Articles, o)
	}
	if SearchSources != nil {
		ps, err := SearchSources(ctx, c.p, query, limit)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			p.Text = clip(p.Text, articleChars)
			out.Sources = append(out.Sources, p)
		}
	}
	return out, nil
}

func createKBDraft(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		Title        string `json:"title"`
		Body         string `json:"body"`
		CollectionID string `json:"collection_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	title := strings.Join(strings.Fields(a.Title), " ")
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return nil, invalid("title must be 1 to 200 characters")
	}
	body := strings.TrimSpace(a.Body)
	if body == "" || utf8.RuneCountInString(body) > 50000 {
		return nil, invalid("body must be 1 to 50,000 characters")
	}
	req := map[string]any{"title": title, "body_html": ai.MarkdownToHTML(body), "status": "draft"}
	if a.CollectionID != "" {
		id, err := idRef(a.CollectionID, "collection_id")
		if err != nil {
			return nil, err
		}
		req["collection_id"] = id
	}
	var ar apiArticle
	if err := c.do(ctx, http.MethodPost, "/v1/kb/articles", nil, req, &ar); err != nil {
		return nil, err
	}
	return outArticle{ID: ar.ID, Title: ar.Title, Slug: ar.Slug, Status: ar.Status, CollectionID: ar.CollectionID, UpdatedAt: ar.UpdatedAt}, nil
}

// --- reports -------------------------------------------------------------------------

func reportTools() []*tool {
	return []*tool{
		{
			// The overview route reads tickets, so the key needs tickets:read as well.
			Name: "get_report", Title: "Get the overview report", Scopes: []string{auth.ScopeSettingsRead, auth.ScopeTicketsRead}, ReadOnly: true, Idempotent: true,
			Description: "Helpdesk numbers for a date range: tickets created and solved, open now, median first response and resolution minutes, " +
				"CSAT, SLA breaches, and breakdowns by day, channel, severity and agent. Defaults to the last 30 days.",
			Schema: obj(map[string]any{
				"from": str("First day, YYYY-MM-DD in the workspace's timezone (default 30 days ago)."),
				"to":   str("Last day, inclusive, YYYY-MM-DD (default today)."),
			}),
			Run: getReport,
		},
	}
}

// reportDays is the most daily rows returned; the totals carry the answer.
const reportDays = 92

func getReport(ctx context.Context, c *caller, raw json.RawMessage) (any, error) {
	a, err := decode[struct {
		From string `json:"from"`
		To   string `json:"to"`
	}](raw)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range map[string]string{"from": a.From, "to": a.To} {
		if v = strings.TrimSpace(v); v != "" {
			if _, err := day(v, k, false); err != nil {
				return nil, err
			}
			q.Set(k, v)
		}
	}
	var rep map[string]any
	if err := c.get(ctx, "/v1/reports/overview", q, &rep); err != nil {
		return nil, err
	}
	if days, ok := rep["by_day"].([]any); ok && len(days) > reportDays {
		rep["by_day"] = days[len(days)-reportDays:]
		rep["by_day_note"] = "the last " + strconv.Itoa(reportDays) + " days of the range"
	}
	if from, to := a.From, a.To; from != "" || to != "" {
		rep["range"] = map[string]string{"from": from, "to": to}
	}
	return rep, nil
}
