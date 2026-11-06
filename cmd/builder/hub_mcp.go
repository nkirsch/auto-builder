package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	boardmcp "github.com/nkirsch/auto-builder/internal/mcp"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// mcpEndpoint serves /mcp by token. The prefix alone chooses: a run token gets
// its one board's agent tools, and only an OAuth token reaches a person's tools.
func (h *hub) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		h.oauthChallenge(w, false)
		return
	}
	if !strings.HasPrefix(token, tenant.OAuthAccessPrefix) {
		h.runner(func(rt *tenantRT, _ int, w http.ResponseWriter, r *http.Request) {
			rt.mcp.ServeHTTP(w, r)
		})(w, r)
		return
	}
	identity, err := h.reg.OAuthAccess(r.Context(), token)
	if errors.Is(err, db.ErrNotFound) {
		h.oauthChallenge(w, true)
		return
	}
	if err != nil {
		slog.Error("oauth access", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.personTools(identity) },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}).ServeHTTP(w, r)
}

// personTools are one signed-in person's tools. Each call resolves their role
// afresh, so removing a member takes effect at once, and the rules are the
// board's own: boards they aren't on don't exist for them.
func (h *hub) personTools(identity string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "auto-builder-hub", Version: h.cfg.GitSHA}, nil)
	p := person{h, identity}
	read := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "list_boards", Annotations: read,
		Description: "The boards you're on, with your role; a platform admin also sees the rest, with an empty role."}, p.listBoards)
	mcp.AddTool(s, &mcp.Tool{Name: "overview", Annotations: read,
		Description: "Platform admins: every board's health, version, unreleased commits and failed runs."}, p.overview)
	mcp.AddTool(s, &mcp.Tool{Name: "list_items", Annotations: read,
		Description: "A board's items with their threads. status: open (default), building, merged, shipped, closed or all."}, p.listItems)
	mcp.AddTool(s, &mcp.Tool{Name: "get_item", Annotations: read,
		Description: "One item with its full thread."}, p.getItem)
	mcp.AddTool(s, &mcp.Tool{Name: "file_item",
		Description: "File an ask on a board. approve (admins) lets the agent build it on its next run."}, p.fileItem)
	mcp.AddTool(s, &mcp.Tool{Name: "reply",
		Description: "Reply on an item as yourself. An admin's reply is an instruction to the agent, and on a finished item reopens it."}, p.reply)
	mcp.AddTool(s, &mcp.Tool{Name: "close_item",
		Description: "Close an item you filed, or any item as an admin."}, p.closeItem)
	mcp.AddTool(s, &mcp.Tool{Name: "reopen_item",
		Description: "Reopen an item you filed, or any item as an admin."}, p.reopenItem)
	mcp.AddTool(s, &mcp.Tool{Name: "approve_item",
		Description: "Admins: approve an item so the agent builds it on its next run, or withdraw approval."}, p.approveItem)
	mcp.AddTool(s, &mcp.Tool{Name: "unblock_item",
		Description: "Admins: clear an item's blocked flag so the agent picks it up again."}, p.unblockItem)
	mcp.AddTool(s, &mcp.Tool{Name: "start_run",
		Description: "Admins: start the board's agent now. It spends money and may merge PRs; confirm with the user first."}, p.startRun)
	mcp.AddTool(s, &mcp.Tool{Name: "release",
		Description: "Admins: cut a release at the head of the board's repo, which deploys to production. Confirm with the user first."}, p.release)
	return s
}

type person struct {
	h        *hub
	identity string
}

// on is the board and this person's member there; anything short of membership
// reads the same as no such board, so slugs can't be probed.
func (p person) on(ctx context.Context, slug string) (*tenantRT, db.Member, error) {
	notOn := fmt.Errorf("you aren't on a board %q", slug)
	if !tenant.WellFormed(slug) {
		return nil, db.Member{}, notOn
	}
	t, err := p.h.reg.Tenant(ctx, slug)
	if errors.Is(err, db.ErrNotFound) || (err == nil && t.Status == "deleted") {
		return nil, db.Member{}, notOn
	}
	if err != nil {
		return nil, db.Member{}, err
	}
	rt, err := p.h.tenantFor(ctx, slug)
	if err != nil {
		return nil, db.Member{}, err
	}
	m, err := rt.b.Auth.Resolve(ctx, p.identity)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			slog.Warn("mcp member", "tenant", slug, "err", err)
		}
		return nil, db.Member{}, notOn
	}
	return rt, m, nil
}

// admin is on, for admins only.
func (p person) admin(ctx context.Context, slug string) (*tenantRT, db.Member, error) {
	rt, m, err := p.on(ctx, slug)
	if err != nil {
		return nil, m, err
	}
	if !m.IsAdmin() {
		return nil, m, fmt.Errorf("%w: only an admin of %s can do that", board.ErrForbidden, slug)
	}
	return rt, m, nil
}

type boardOut struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Role   string `json:"role" jsonschema:"admin or member; empty for a platform admin who isn't on it"`
	Status string `json:"status"`
	Board  string `json:"board"`
	App    string `json:"app"`
}

type boardsOut struct {
	Identity string     `json:"identity"`
	Boards   []boardOut `json:"boards"`
}

func (p person) listBoards(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, boardsOut, error) {
	tenants, err := p.h.reg.Tenants(ctx)
	if err != nil {
		return nil, boardsOut{}, err
	}
	_, perr := p.h.reg.PlatformAdmin(ctx, p.identity)
	out := boardsOut{Identity: p.identity, Boards: []boardOut{}}
	for _, t := range tenants {
		if t.Status == "deleted" {
			continue
		}
		st, err := p.h.reg.Store(ctx, t.Slug)
		if err != nil {
			return nil, boardsOut{}, err
		}
		role := roleOf(ctx, st, p.identity)
		if role == "" && perr != nil {
			continue
		}
		out.Boards = append(out.Boards, boardOut{Slug: t.Slug, Name: t.Name, Role: role, Status: t.Status,
			Board: p.h.cfg.BaseURL + "/" + t.Slug + "/", App: p.h.appURL(t.Slug)})
	}
	return nil, out, nil
}

func (p person) overview(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, hubOverview, error) {
	if _, err := p.h.reg.PlatformAdmin(ctx, p.identity); err != nil {
		return nil, hubOverview{}, fmt.Errorf("%w: platform admins only", board.ErrForbidden)
	}
	o, err := p.h.overview(ctx)
	return nil, o, err
}

type personListIn struct {
	Board  string `json:"board" jsonschema:"the board's slug, from list_boards"`
	Status string `json:"status,omitempty" jsonschema:"open (default), building, merged, shipped, closed or all"`
}

func (p person) listItems(ctx context.Context, _ *mcp.CallToolRequest, in personListIn) (*mcp.CallToolResult, boardmcp.ListOut, error) {
	rt, _, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, boardmcp.ListOut{}, err
	}
	out, err := rt.tools.Items(ctx, in.Status)
	return nil, out, err
}

type itemIn struct {
	Board string `json:"board" jsonschema:"the board's slug, from list_boards"`
	ID    int    `json:"id"`
}

func (p person) getItem(ctx context.Context, _ *mcp.CallToolRequest, in itemIn) (*mcp.CallToolResult, boardmcp.ItemOut, error) {
	rt, _, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, boardmcp.ItemOut{}, err
	}
	out, err := rt.tools.Item(ctx, in.ID)
	return nil, out, err
}

type personFileIn struct {
	Board   string `json:"board" jsonschema:"the board's slug, from list_boards"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
	Approve bool   `json:"approve,omitempty" jsonschema:"admins: approve it at once"`
}

type fileOut struct {
	ID       int    `json:"id"`
	URL      string `json:"url"`
	Approved bool   `json:"approved"`
}

func (p person) fileItem(ctx context.Context, _ *mcp.CallToolRequest, in personFileIn) (*mcp.CallToolResult, fileOut, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, fileOut{}, err
	}
	if in.Approve && !m.IsAdmin() {
		return nil, fileOut{}, fmt.Errorf("%w: only an admin can approve", board.ErrForbidden)
	}
	id, err := rt.b.File(ctx, m.Login, in.Title, in.Body, in.Approve)
	if err != nil {
		return nil, fileOut{}, err
	}
	return nil, fileOut{ID: id, URL: rt.b.ItemURL(id), Approved: in.Approve}, nil
}

type personReplyIn struct {
	Board string `json:"board" jsonschema:"the board's slug, from list_boards"`
	ID    int    `json:"id"`
	Body  string `json:"body" jsonschema:"markdown; @login notifies that member"`
}

type done struct {
	OK bool `json:"ok"`
}

func (p person) reply(ctx context.Context, _ *mcp.CallToolRequest, in personReplyIn) (*mcp.CallToolResult, done, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, done{}, err
	}
	if err := rt.b.Reply(ctx, m, in.ID, in.Body); err != nil {
		return nil, done{}, err
	}
	return nil, done{true}, nil
}

type personCloseIn struct {
	Board string `json:"board" jsonschema:"the board's slug, from list_boards"`
	ID    int    `json:"id"`
	Note  string `json:"note,omitempty"`
}

func (p person) closeItem(ctx context.Context, _ *mcp.CallToolRequest, in personCloseIn) (*mcp.CallToolResult, done, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, done{}, err
	}
	if err := rt.b.CloseItem(ctx, m, in.ID, in.Note); err != nil {
		return nil, done{}, err
	}
	return nil, done{true}, nil
}

func (p person) reopenItem(ctx context.Context, _ *mcp.CallToolRequest, in itemIn) (*mcp.CallToolResult, done, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, done{}, err
	}
	if err := rt.b.ReopenItem(ctx, m, in.ID); err != nil {
		return nil, done{}, err
	}
	return nil, done{true}, nil
}

type approveIn struct {
	Board    string `json:"board" jsonschema:"the board's slug, from list_boards"`
	ID       int    `json:"id"`
	Approved bool   `json:"approved" jsonschema:"true to approve, false to withdraw approval"`
}

func (p person) approveItem(ctx context.Context, _ *mcp.CallToolRequest, in approveIn) (*mcp.CallToolResult, done, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, done{}, err
	}
	if err := rt.b.Approve(ctx, m, in.ID, in.Approved); err != nil {
		return nil, done{}, err
	}
	return nil, done{true}, nil
}

func (p person) unblockItem(ctx context.Context, _ *mcp.CallToolRequest, in itemIn) (*mcp.CallToolResult, done, error) {
	rt, m, err := p.on(ctx, in.Board)
	if err != nil {
		return nil, done{}, err
	}
	if err := rt.b.Unblock(ctx, m, in.ID); err != nil {
		return nil, done{}, err
	}
	return nil, done{true}, nil
}

type slugIn struct {
	Board string `json:"board" jsonschema:"the board's slug, from list_boards"`
}

type runOut struct {
	Run  int    `json:"run"`
	Runs string `json:"runs"`
}

func (p person) startRun(ctx context.Context, _ *mcp.CallToolRequest, in slugIn) (*mcp.CallToolResult, runOut, error) {
	rt, m, err := p.admin(ctx, in.Board)
	if err != nil {
		return nil, runOut{}, err
	}
	id, err := rt.gate.RunNow(ctx, m.Login)
	if err != nil {
		return nil, runOut{}, err
	}
	slog.Info("mcp run", "tenant", in.Board, "by", p.identity, "run", id)
	return nil, runOut{id, p.h.cfg.BaseURL + "/" + in.Board + "/runs"}, nil
}

type releaseOut struct {
	Tag     string `json:"tag" jsonschema:"empty when there was nothing to release"`
	Commits int    `json:"commits"`
}

func (p person) release(ctx context.Context, _ *mcp.CallToolRequest, in slugIn) (*mcp.CallToolResult, releaseOut, error) {
	rt, _, err := p.admin(ctx, in.Board)
	if err != nil {
		return nil, releaseOut{}, err
	}
	if _, ok := rt.b.PendingRelease(ctx); !ok {
		return nil, releaseOut{}, fmt.Errorf("%s doesn't deploy on release", in.Board)
	}
	if rt.b.Ready != nil && !rt.b.Ready(ctx) {
		return nil, releaseOut{}, fmt.Errorf("%s isn't set up yet", in.Board)
	}
	tag, ahead, err := rt.b.Release(ctx)
	if err != nil {
		return nil, releaseOut{}, err
	}
	slog.Info("mcp release", "tenant", in.Board, "by", p.identity, "tag", tag, "commits", ahead)
	return nil, releaseOut{tag, ahead}, nil
}
