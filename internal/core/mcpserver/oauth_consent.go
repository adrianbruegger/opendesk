package mcpserver

// The signed-in side of OAuth for the MCP server: the consent page's API (look up a
// pending authorization request, approve it for a workspace or deny it), and
// Settings > AI clients (the server's URL and tools, the connected clients, revoke).

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/audit"
	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/core/oauthgrant"
	"github.com/useopendesk/opendesk/internal/crypto"
	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/httpx"
	"github.com/useopendesk/opendesk/internal/ids"
)

// OAuthClientInfo is what the consent page shows about the client asking for access.
type OAuthClientInfo struct {
	Name         string `json:"name" doc:"The name the client registered with"`
	URI          string `json:"uri" doc:"The client's https home page, or empty"`
	RedirectHost string `json:"redirect_host" doc:"Where the browser returns after the decision"`
	Local        bool   `json:"local" doc:"The redirect goes to an app on this computer (a loopback address)"`
}

// OAuthWorkspaceChoice is a workspace the user may connect, with the scopes their role
// there allows out of the requested ones.
type OAuthWorkspaceChoice struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Role   auth.Role `json:"role"`
	Scopes []string  `json:"scopes" doc:"The requested scopes this role can use; empty means the role can use none"`
}

// OAuthRequest is a pending authorization request.
type OAuthRequest struct {
	ID         uuid.UUID              `json:"id"`
	Client     OAuthClientInfo        `json:"client"`
	Scopes     []string               `json:"scopes" doc:"The scopes the client asked for"`
	Workspaces []OAuthWorkspaceChoice `json:"workspaces"`
	ExpiresAt  time.Time              `json:"expires_at"`
}

// OAuthDecision tells the web app where to send the browser.
type OAuthDecision struct {
	RedirectTo string `json:"redirect_to" doc:"The client's redirect URI with the code (or the error) and state"`
}

// AIClientTool is one tool the MCP server offers.
type AIClientTool struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes" doc:"All are needed for the tool to be offered"`
	ReadOnly    bool     `json:"read_only"`
	Destructive bool     `json:"destructive"`
	PeopleOnly  bool     `json:"people_only" doc:"Offered only to clients connected with a sign-in (OAuth), never to API keys"`
}

// AIClientServer describes the MCP server for Settings > AI clients.
type AIClientServer struct {
	URL                 string         `json:"url" doc:"The MCP endpoint, {API_URL}/mcp"`
	ResourceMetadata    string         `json:"resource_metadata" doc:"Protected Resource Metadata (RFC 9728)"`
	AuthorizationServer string         `json:"authorization_server" doc:"The OAuth issuer; its metadata is at /.well-known/oauth-authorization-server"`
	Scopes              []string       `json:"scopes" doc:"Scopes an AI client may be granted over OAuth"`
	Tools               []AIClientTool `json:"tools"`
}

// AIClientConnection is an AI client a member connected with OAuth.
type AIClientConnection struct {
	ID         uuid.UUID       `json:"id"`
	Client     OAuthClientInfo `json:"client"`
	User       *domain.UserRef `json:"user"`
	Mine       bool            `json:"mine" doc:"The caller connected it"`
	Scopes     []string        `json:"scopes"`
	CreatedAt  time.Time       `json:"created_at"`
	LastUsedAt *time.Time      `json:"last_used_at"`
}

// approvePerMinute bounds consent decisions per user.
const approvePerMinute = 30

func errRequestGone() *httpx.Error {
	return httpx.E(http.StatusGone, "request_expired", "This request has expired or was already answered. Start again from your AI client.")
}

func clientInfo(name, uri, redirect string) OAuthClientInfo {
	ci := OAuthClientInfo{Name: name, URI: uri}
	if u, err := url.Parse(redirect); err == nil {
		ci.RedirectHost = u.Host
		ci.Local = u.Scheme == "http" && loopback(u.Hostname())
	}
	return ci
}

// requireMember demands a signed-in member of the selected workspace (no API keys, no
// OAuth grants).
func requireMember(ctx context.Context) (*auth.Principal, error) {
	p, err := auth.RequireWorkspace(ctx, auth.RoleViewer, "")
	if err != nil {
		return nil, err
	}
	if p.Kind != auth.KindUser {
		return nil, auth.ErrForbidden
	}
	return p, nil
}

type pendingRequest struct {
	ID, ClientID               uuid.UUID
	Redirect, State, Name, URI string
	Scopes                     []string
	ExpiresAt                  time.Time
}

// loadPending reads a request that still waits for a decision. With tx it locks the row.
func loadPending(ctx context.Context, q db.Querier, id uuid.UUID, lock bool) (*pendingRequest, error) {
	sql := `SELECT a.id, a.client_id, a.redirect_uri, a.state, a.scopes, a.expires_at, c.name, c.uri
		FROM oauth_authorizations a JOIN oauth_clients c ON c.id = a.client_id
		WHERE a.id = $1 AND a.status = 'pending' AND a.expires_at > now()`
	if lock {
		sql += ` FOR UPDATE OF a`
	}
	pr := &pendingRequest{}
	err := q.QueryRow(ctx, sql, id).Scan(&pr.ID, &pr.ClientID, &pr.Redirect, &pr.State, &pr.Scopes, &pr.ExpiresAt, &pr.Name, &pr.URI)
	if db.IsNotFound(err) {
		return nil, errRequestGone()
	}
	return pr, err
}

func (o *oauth) registerAPI(api huma.API) {
	tags := []string{"AI clients"}

	huma.Register(api, huma.Operation{OperationID: "get-oauth-request", Method: http.MethodGet, Path: "/v1/oauth/requests/{id}",
		Summary: "Get an authorization request", Description: "The consent page reads what an AI client asks for and the workspaces the signed-in user can connect.",
		Tags: tags},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body OAuthRequest }, error) {
			p, err := auth.RequireUser(ctx)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			id, err := uuid.Parse(in.ID)
			if err != nil {
				return nil, errRequestGone()
			}
			pr, err := loadPending(ctx, o.d.DB.Pool, id, false)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			rows, err := o.d.DB.Pool.Query(ctx, `SELECT w.id, w.name, m.role FROM members m JOIN workspaces w ON w.id = m.workspace_id
				WHERE m.user_id = $1 ORDER BY lower(w.name), w.id`, p.UserID)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			choices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OAuthWorkspaceChoice, error) {
				var c OAuthWorkspaceChoice
				err := r.Scan(&c.ID, &c.Name, &c.Role)
				c.Scopes = oauthgrant.Effective(pr.Scopes, c.Role)
				return c, err
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body OAuthRequest }{OAuthRequest{ID: pr.ID, Client: clientInfo(pr.Name, pr.URI, pr.Redirect),
				Scopes: pr.Scopes, Workspaces: choices, ExpiresAt: pr.ExpiresAt}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "approve-oauth-request", Method: http.MethodPost, Path: "/v1/oauth/requests/{id}/approve",
		Summary: "Approve an authorization request", Description: "Connects the AI client to the chosen workspace with the requested scopes the user's role allows, " +
			"and answers with the client's redirect URI carrying a single-use code (60 seconds).", Tags: tags},
		func(ctx context.Context, in *struct {
			ID   string `path:"id"`
			Body struct {
				WorkspaceID uuid.UUID `json:"workspace_id"`
			}
		}) (*struct {
			CacheControl string `header:"Cache-Control"`
			Body         OAuthDecision
		}, error) {
			p, err := auth.RequireUser(ctx)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			id, err := uuid.Parse(in.ID)
			if err != nil {
				return nil, errRequestGone()
			}
			if !o.d.Limiter.Allow("mcp:oauth:decide:"+p.UserID.String(), approvePerMinute, time.Minute) {
				return nil, httpx.RateLimited()
			}
			wid := in.Body.WorkspaceID
			var redirect string
			var grantID uuid.UUID
			var scopes []string
			err = o.d.DB.Tx(ctx, func(tx pgx.Tx) error {
				pr, err := loadPending(ctx, tx, id, true)
				if err != nil {
					return err
				}
				var role auth.Role
				if err := tx.QueryRow(ctx, `SELECT role FROM members WHERE workspace_id = $1 AND user_id = $2`, wid, p.UserID).Scan(&role); err != nil {
					if db.IsNotFound(err) {
						return httpx.NotFound("Workspace")
					}
					return err
				}
				scopes = oauthgrant.Effective(pr.Scopes, role)
				if len(scopes) == 0 {
					return httpx.E(http.StatusUnprocessableEntity, "no_scopes", "Your role in this workspace can't use any of the access this app asks for.")
				}
				grantID = ids.New()
				if _, err := tx.Exec(ctx, `INSERT INTO oauth_grants (id, workspace_id, user_id, client_id, scopes, resource)
					VALUES ($1, $2, $3, $4, $5, (SELECT resource FROM oauth_authorizations WHERE id = $6))`,
					grantID, wid, p.UserID, pr.ClientID, scopes, pr.ID); err != nil {
					return err
				}
				code := crypto.RandomToken(32)
				if _, err := tx.Exec(ctx, `UPDATE oauth_authorizations SET status = 'approved', user_id = $2, workspace_id = $3, grant_id = $4,
					code_hash = $5, decided_at = now(), expires_at = $6 WHERE id = $1`,
					pr.ID, p.UserID, wid, grantID, crypto.HashToken(code), time.Now().Add(codeTTL)); err != nil {
					return err
				}
				ci := clientInfo(pr.Name, pr.URI, pr.Redirect)
				if err := audit.Log(ctx, tx, p, wid, "oauth.grant_created", "oauth_grant", &grantID,
					map[string]any{"client": pr.Name, "scopes": scopes, "redirect_host": ci.RedirectHost}); err != nil {
					return err
				}
				redirect = withParams(pr.Redirect, map[string]string{"code": code, "state": pr.State, "iss": o.issuer()})
				return nil
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			o.d.Analytics.Capture(p.UserID.String(), wid.String(), "ai_client_connected", map[string]any{"scope_count": len(scopes)})
			return &struct {
				CacheControl string `header:"Cache-Control"`
				Body         OAuthDecision
			}{"no-store", OAuthDecision{RedirectTo: redirect}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "deny-oauth-request", Method: http.MethodPost, Path: "/v1/oauth/requests/{id}/deny",
		Summary: "Deny an authorization request", Description: "Answers with the client's redirect URI carrying error=access_denied and state.", Tags: tags},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body OAuthDecision }, error) {
			p, err := auth.RequireUser(ctx)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			id, err := uuid.Parse(in.ID)
			if err != nil {
				return nil, errRequestGone()
			}
			if !o.d.Limiter.Allow("mcp:oauth:decide:"+p.UserID.String(), approvePerMinute, time.Minute) {
				return nil, httpx.RateLimited()
			}
			var redirect string
			err = o.d.DB.Tx(ctx, func(tx pgx.Tx) error {
				pr, err := loadPending(ctx, tx, id, true)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE oauth_authorizations SET status = 'denied', user_id = $2, decided_at = now() WHERE id = $1`,
					pr.ID, p.UserID); err != nil {
					return err
				}
				redirect = withParams(pr.Redirect, map[string]string{"error": "access_denied",
					"error_description": "The user denied access.", "state": pr.State, "iss": o.issuer()})
				return nil
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body OAuthDecision }{OAuthDecision{RedirectTo: redirect}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "get-ai-client-server", Method: http.MethodGet, Path: "/v1/ai-clients/server",
		Summary: "Describe the MCP server", Description: "The MCP endpoint, its OAuth metadata and the tools it offers, for setting up AI clients.", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body AIClientServer }, error) {
			if _, err := requireMember(ctx); err != nil {
				return nil, httpx.Wrap(err)
			}
			out := AIClientServer{URL: Resource(o.d), ResourceMetadata: ResourceMetadataURL(o.d), AuthorizationServer: o.issuer(),
				Scopes: oauthgrant.Scopes, Tools: []AIClientTool{}}
			for _, t := range catalog() {
				out.Tools = append(out.Tools, AIClientTool{Name: t.Name, Title: t.Title, Description: t.Description, Scopes: t.Scopes,
					ReadOnly: t.ReadOnly, Destructive: t.Destructive, PeopleOnly: t.Person})
			}
			return &struct{ Body AIClientServer }{out}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "list-ai-client-connections", Method: http.MethodGet, Path: "/v1/ai-clients/connections",
		Summary: "List connected AI clients", Description: "AI clients connected to the workspace with OAuth: the caller's own, or every member's for admins.", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Data []AIClientConnection `json:"data"`
			}
		}, error) {
			p, err := requireMember(ctx)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			all := p.Role.AtLeast(auth.RoleAdmin)
			rows, err := o.d.DB.Pool.Query(ctx, `SELECT g.id, c.name, c.uri, c.redirect_uris[1], g.scopes, g.created_at, g.last_used_at,
				u.id, u.name, u.email, u.avatar_url
				FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id JOIN users u ON u.id = g.user_id
				WHERE g.workspace_id = $1 AND g.revoked_at IS NULL AND g.last_used_at IS NOT NULL AND ($2 OR g.user_id = $3)
				ORDER BY g.last_used_at DESC, g.id LIMIT 200`, p.WorkspaceID, all, p.UserID)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AIClientConnection, error) {
				var c AIClientConnection
				var name, uri, redirect, avatar string
				u := domain.UserRef{}
				if err := r.Scan(&c.ID, &name, &uri, &redirect, &c.Scopes, &c.CreatedAt, &c.LastUsedAt, &u.ID, &u.Name, &u.Email, &avatar); err != nil {
					return c, err
				}
				u.AvatarURL = domain.Avatar(avatar)
				c.Client, c.User, c.Mine = clientInfo(name, uri, redirect), &u, u.ID == p.UserID
				return c, nil
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			out := &struct {
				Body struct {
					Data []AIClientConnection `json:"data"`
				}
			}{}
			out.Body.Data = list
			if out.Body.Data == nil {
				out.Body.Data = []AIClientConnection{}
			}
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revoke-ai-client-connection", Method: http.MethodDelete, Path: "/v1/ai-clients/connections/{id}",
		Summary: "Disconnect an AI client", Description: "Revokes the grant and every token issued under it. Members revoke their own; admins any.",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{}, error) {
			p, err := requireMember(ctx)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			id, err := uuid.Parse(in.ID)
			if err != nil {
				return nil, httpx.NotFound("Connection")
			}
			err = o.d.DB.Tx(ctx, func(tx pgx.Tx) error {
				var owner uuid.UUID
				if err := tx.QueryRow(ctx, `SELECT user_id FROM oauth_grants WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL FOR UPDATE`,
					id, p.WorkspaceID).Scan(&owner); err != nil {
					if db.IsNotFound(err) {
						return httpx.NotFound("Connection")
					}
					return err
				}
				if owner != p.UserID && !p.Role.AtLeast(auth.RoleAdmin) {
					return httpx.NotFound("Connection")
				}
				_, err := oauthgrant.Revoke(ctx, tx, p, uuid.Nil, p.WorkspaceID, id, oauthgrant.ReasonUser)
				return err
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return nil, nil
		})
}
