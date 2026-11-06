package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk0"

func testChallenge() string {
	s := sha256.Sum256([]byte(testVerifier))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// mcpCall posts one JSON-RPC call to /mcp with token.
func mcpCall(t *testing.T, srv http.Handler, token, method string, params any) (int, http.Header, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	r.Host = "hub.example"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var out map[string]any
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
	}
	return w.Code, w.Header(), out
}

func toolNames(t *testing.T, srv http.Handler, token string) string {
	t.Helper()
	code, _, out := mcpCall(t, srv, token, "tools/list", map[string]any{})
	if code != 200 {
		t.Fatalf("tools/list: %d", code)
	}
	var names []string
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return " " + strings.Join(names, " ") + " "
}

// call runs a tool, returning its structured result, or its error's text.
func call(t *testing.T, srv http.Handler, token, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	code, _, out := mcpCall(t, srv, token, "tools/call", map[string]any{"name": tool, "arguments": args})
	if code != 200 {
		t.Fatalf("%s: %d", tool, code)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: %v", tool, out)
	}
	if res["isError"] == true {
		return nil, res["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	m, _ := res["structuredContent"].(map[string]any)
	return m, ""
}

func postJSON(srv http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Host = "hub.example"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func registerClient(t *testing.T, srv http.Handler, redirect string) string {
	t.Helper()
	w := postJSON(srv, "/oauth/register", `{"client_name":"Claude Code <b>","redirect_uris":["`+redirect+`"]}`)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 201 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	return out["client_id"].(string)
}

func authorizeForm(client, redirect string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {redirect}, "state": {"xyz"}, "response_type": {"code"},
		"code_challenge": {testChallenge()}, "code_challenge_method": {"S256"}, "resource": {"https://hub.example/mcp"}, "decision": {"allow"}}
}

func getCode(t *testing.T, h *hub, srv http.Handler, identity, client, redirect string) string {
	t.Helper()
	w := do(h, srv, identity, "POST", "/oauth/authorize", authorizeForm(client, redirect))
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != 302 || u.Query().Get("code") == "" || u.Query().Get("state") != "xyz" {
		t.Fatalf("authorize: %d %q", w.Code, w.Header().Get("Location"))
	}
	return u.Query().Get("code")
}

func tokenCall(srv http.Handler, form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Host = "hub.example"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func exchange(t *testing.T, srv http.Handler, client, redirect, code string) (access, refresh string) {
	t.Helper()
	w, out := tokenCall(srv, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {testVerifier}, "resource": {"https://hub.example/mcp"}})
	if w.Code != 200 {
		t.Fatalf("token: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("token response may be cached")
	}
	return out["access_token"].(string), out["refresh_token"].(string)
}

func TestOAuthFlow(t *testing.T) {
	h := testHub(t)
	srv := h.routes()
	const redirect = "http://localhost:4000/callback"

	// A client finds its way from /mcp's 401 to the endpoints.
	code, hdr, _ := mcpCall(t, srv, "", "tools/list", map[string]any{})
	if code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), `resource_metadata="https://hub.example/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("challenge: %d %q", code, hdr.Get("WWW-Authenticate"))
	}
	for path, want := range map[string]string{
		"/.well-known/oauth-protected-resource/mcp": `"resource":"https://hub.example/mcp"`,
		"/.well-known/oauth-protected-resource":     `"authorization_servers":["https://hub.example"]`,
		"/.well-known/oauth-authorization-server":   `"code_challenge_methods_supported":["S256"]`,
	} {
		if w := do(h, srv, "", "GET", path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}

	// Only https, or http on loopback, without anything to smuggle the code.
	for _, bad := range []string{"http://evil.example/cb", "https://evil.example/cb#x", "https://me@evil.example/cb", "myapp://cb", "javascript:alert(1)", "http://localhost.evil.example/cb"} {
		if w := postJSON(srv, "/oauth/register", `{"redirect_uris":["`+bad+`"]}`); w.Code != 400 {
			t.Errorf("registered %s: %d", bad, w.Code)
		}
	}
	client := registerClient(t, srv, redirect)
	q := authorizeForm(client, redirect)

	// Signed out goes to sign in and comes back.
	if w := do(h, srv, "", "GET", "/oauth/authorize?"+q.Encode(), nil); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/auth/login?next=%2Foauth%2Fauthorize") {
		t.Fatalf("signed out: %d %q", w.Code, w.Header().Get("Location"))
	}
	// Consent names the (escaped) client and where it returns to, and can't be framed.
	w := do(h, srv, "bob", "GET", "/oauth/authorize?"+q.Encode(), nil)
	if b := w.Body.String(); w.Code != 200 || !strings.Contains(b, "Claude Code &lt;b&gt;") || !strings.Contains(b, "<strong>localhost</strong>") || !strings.Contains(b, "<strong>bob</strong>") {
		t.Fatalf("consent: %d %.600s", w.Code, b)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("consent can be framed")
	}

	// Nothing redirects until the client and redirect check out.
	for name, change := range map[string]url.Values{
		"unknown client":      {"client_id": {"nope"}},
		"other redirect":      {"redirect_uri": {"https://evil.example/cb"}},
		"other loopback":      {"redirect_uri": {"http://localhost:4000/other"}},
		"other loopback host": {"redirect_uri": {"http://127.0.0.1:4000/callback"}},
	} {
		bad := authorizeForm(client, redirect)
		for k, v := range change {
			bad[k] = v
		}
		if w := do(h, srv, "bob", "GET", "/oauth/authorize?"+bad.Encode(), nil); w.Code != 400 || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Location"))
		}
	}
	// Past that, errors go back to the client.
	for name, change := range map[string]url.Values{
		"no PKCE":        {"code_challenge": {""}},
		"plain PKCE":     {"code_challenge_method": {"plain"}},
		"other resource": {"resource": {"https://other.example/mcp"}},
		"token flow":     {"response_type": {"token"}},
	} {
		bad := authorizeForm(client, redirect)
		for k, v := range change {
			bad[k] = v
		}
		w := do(h, srv, "bob", "GET", "/oauth/authorize?"+bad.Encode(), nil)
		if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), redirect+"?error=") {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Location"))
		}
	}

	// Consent can't be forged from another site or by the signed-out.
	r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(q.Encode()))
	r.Host = "hub.example"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(h.auth.SessionCookie("bob"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Errorf("cross-origin consent: %d", rec.Code)
	}
	if w := do(h, srv, "", "POST", "/oauth/authorize", q); w.Code == 302 && strings.Contains(w.Header().Get("Location"), "code=") {
		t.Error("signed-out consent issued a code")
	}
	denied := authorizeForm(client, redirect)
	denied.Set("decision", "deny")
	if w := do(h, srv, "bob", "POST", "/oauth/authorize", denied); !strings.Contains(w.Header().Get("Location"), "error=access_denied") {
		t.Errorf("deny: %q", w.Header().Get("Location"))
	}

	// A loopback redirect may use another port, and the code is bound to it.
	moved := "http://localhost:5555/callback"
	if c := getCode(t, h, srv, "bob", client, moved); c == "" {
		t.Error("loopback port")
	}

	// Each way of misusing a code fails, and spends it.
	for name, change := range map[string]url.Values{
		"wrong verifier": {"code_verifier": {strings.Repeat("a", 43)}},
		"no verifier":    {"code_verifier": {""}},
		"wrong redirect": {"redirect_uri": {moved}},
		"wrong client":   {"client_id": {registerClient(t, srv, redirect)}},
	} {
		c := getCode(t, h, srv, "bob", client, redirect)
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {c}, "redirect_uri": {redirect}, "code_verifier": {testVerifier}}
		for k, v := range change {
			form[k] = v
		}
		if w, _ := tokenCall(srv, form); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_grant") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
		form = url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {c}, "redirect_uri": {redirect}, "code_verifier": {testVerifier}}
		if w, _ := tokenCall(srv, form); w.Code != 400 {
			t.Errorf("%s: the code survived: %d", name, w.Code)
		}
	}

	c := getCode(t, h, srv, "bob", client, redirect)
	access, refresh := exchange(t, srv, client, redirect, c)
	if w, _ := tokenCall(srv, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {c}, "redirect_uri": {redirect}, "code_verifier": {testVerifier}}); w.Code != 400 {
		t.Errorf("code reused: %d", w.Code)
	}
	if got, _ := call(t, srv, access, "list_boards", nil); got["identity"] != "bob" {
		t.Errorf("identity %v", got)
	}

	// Refresh rotates; the token types don't substitute for each other.
	if code, _, _ := mcpCall(t, srv, refresh, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("refresh token as access: %d", code)
	}
	if w, _ := tokenCall(srv, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {access}}); w.Code != 400 {
		t.Errorf("access token as refresh: %d", w.Code)
	}
	if w, _ := tokenCall(srv, url.Values{"grant_type": {"refresh_token"}, "client_id": {registerClient(t, srv, redirect)}, "refresh_token": {refresh}}); w.Code != 400 {
		t.Errorf("another client's refresh: %d", w.Code)
	}
	w, out := tokenCall(srv, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}})
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	access2 := out["access_token"].(string)

	// A concurrent client's reuse is refused but ends nothing...
	if w, _ := tokenCall(srv, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}}); w.Code != 400 {
		t.Errorf("reuse: %d", w.Code)
	}
	if code, _, _ := mcpCall(t, srv, access2, "tools/list", map[string]any{}); code != 200 {
		t.Errorf("grace reuse ended the grant: %d", code)
	}
	// ...but later it means theft, and ends the grant.
	if _, err := h.reg.Control.Exec(context.Background(), `UPDATE oauth_tokens SET used_at = now() - interval '1 hour' WHERE used_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if w, _ := tokenCall(srv, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}}); w.Code != 400 {
		t.Errorf("late reuse: %d", w.Code)
	}
	code, hdr, _ = mcpCall(t, srv, access2, "tools/list", map[string]any{})
	if code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("late reuse left the grant alive: %d %q", code, hdr.Get("WWW-Authenticate"))
	}

	// Expired tokens stop working.
	access3, _ := exchange(t, srv, client, redirect, getCode(t, h, srv, "bob", client, redirect))
	if _, err := h.reg.Control.Exec(context.Background(), `UPDATE oauth_tokens SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, srv, access3, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("expired access: %d", code)
	}
}

func TestPersonTools(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	const redirect = "http://localhost:4000/callback"
	if w := do(h, srv, "nick@example.org", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	st, err := h.reg.Store(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMember(ctx, "bob", "member", "test"); err != nil {
		t.Fatal(err)
	}
	client := registerClient(t, srv, redirect)
	tokenFor := func(identity string) string {
		a, _ := exchange(t, srv, client, redirect, getCode(t, h, srv, identity, client, redirect))
		return a
	}
	admin, bob, mallory := tokenFor("nick@example.org"), tokenFor("bob"), tokenFor("mallory")

	// The token decides the tools: a build never reaches a person's, nor a person a build's.
	run, err := h.reg.IssueRunToken(ctx, "alpha", 1)
	if err != nil {
		t.Fatal(err)
	}
	build := toolNames(t, srv, run)
	if !strings.Contains(build, " set_pr ") || strings.Contains(build, " release ") || strings.Contains(build, " start_run ") || strings.Contains(build, " list_boards ") {
		t.Errorf("run token's tools: %s", build)
	}
	person := toolNames(t, srv, admin)
	if !strings.Contains(person, " release ") || !strings.Contains(person, " list_boards ") || strings.Contains(person, " set_pr ") {
		t.Errorf("person's tools: %s", person)
	}
	if code, _, _ := mcpCall(t, srv, "hat_"+run, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("run token dressed as OAuth: %d", code)
	}

	// Boards you aren't on don't exist for you, the same as ones that don't.
	if got, _ := call(t, srv, bob, "list_boards", nil); len(got["boards"].([]any)) != 1 {
		t.Errorf("bob's boards: %v", got)
	}
	if got, _ := call(t, srv, mallory, "list_boards", nil); len(got["boards"].([]any)) != 0 {
		t.Errorf("mallory's boards: %v", got)
	}
	_, onMissing := call(t, srv, mallory, "list_items", map[string]any{"board": "nosuch"})
	_, onAlpha := call(t, srv, mallory, "list_items", map[string]any{"board": "alpha"})
	if onMissing == "" || strings.ReplaceAll(onAlpha, "alpha", "nosuch") != onMissing {
		t.Errorf("mallory can probe slugs: %q vs %q", onAlpha, onMissing)
	}
	for tool, args := range map[string]map[string]any{
		"get_item":     {"board": "alpha", "id": 1},
		"file_item":    {"board": "alpha", "title": "x"},
		"reply":        {"board": "alpha", "id": 1, "body": "x"},
		"close_item":   {"board": "alpha", "id": 1},
		"reopen_item":  {"board": "alpha", "id": 1},
		"approve_item": {"board": "alpha", "id": 1, "approved": true},
		"unblock_item": {"board": "alpha", "id": 1},
		"start_run":    {"board": "alpha"},
		"release":      {"board": "alpha"},
	} {
		if _, e := call(t, srv, mallory, tool, args); !strings.Contains(e, "aren't on a board") {
			t.Errorf("mallory %s: %q", tool, e)
		}
	}

	// A member files and talks, as themselves, but can't approve or ship.
	filed, e := call(t, srv, bob, "file_item", map[string]any{"board": "alpha", "title": "Fix it", "body": "Please"})
	if e != "" || filed["approved"] != false {
		t.Fatalf("file: %v %q", filed, e)
	}
	id := int(filed["id"].(float64))
	if _, e := call(t, srv, bob, "file_item", map[string]any{"board": "alpha", "title": "Sneak", "approve": true}); !strings.Contains(e, "admin") {
		t.Errorf("member approved at filing: %q", e)
	}
	if _, e := call(t, srv, bob, "reply", map[string]any{"board": "alpha", "id": id, "body": "More detail"}); e != "" {
		t.Errorf("reply: %q", e)
	}
	item, e := call(t, srv, bob, "get_item", map[string]any{"board": "alpha", "id": id})
	if e != "" {
		t.Fatal(e)
	}
	thread := item["thread"].([]any)
	if len(thread) != 2 || thread[1].(map[string]any)["author"] != "bob" || thread[1].(map[string]any)["role"] != "member" {
		t.Errorf("thread: %v", thread)
	}
	if err := st.SetBlocked(ctx, id, "stuck", false); err != nil {
		t.Fatal(err)
	}
	for tool, args := range map[string]map[string]any{
		"approve_item": {"board": "alpha", "id": id, "approved": true},
		"unblock_item": {"board": "alpha", "id": id},
		"start_run":    {"board": "alpha"},
		"release":      {"board": "alpha"},
		"overview":     {},
	} {
		if _, e := call(t, srv, bob, tool, args); !strings.Contains(e, "not allowed") {
			t.Errorf("member %s: %q", tool, e)
		}
	}
	if it, _ := st.Item(ctx, id); it.Approved || it.Blocked != "stuck" {
		t.Errorf("a refused call took effect: %+v", it)
	}

	// An admin can, and it's recorded as them.
	if _, e := call(t, srv, admin, "approve_item", map[string]any{"board": "alpha", "id": id, "approved": true}); e != "" {
		t.Errorf("approve: %q", e)
	}
	if _, e := call(t, srv, admin, "unblock_item", map[string]any{"board": "alpha", "id": id}); e != "" {
		t.Errorf("unblock: %q", e)
	}
	it, err := st.Item(ctx, id)
	if err != nil || !it.Approved || it.Blocked != "" || it.ApprovedBy != "nkirsch" {
		t.Errorf("after admin: %+v %v", it, err)
	}
	// This hub has no App, so release stops at "doesn't deploy on release" rather than the role check.
	if _, e := call(t, srv, admin, "release", map[string]any{"board": "alpha"}); strings.Contains(e, "not allowed") || e == "" {
		t.Errorf("admin release: %q", e)
	}
	if _, e := call(t, srv, admin, "overview", nil); e != "" {
		t.Errorf("overview: %q", e)
	}

	// The author can close their own; another member can't close it.
	if err := st.UpsertMember(ctx, "carol", "member", "test"); err != nil {
		t.Fatal(err)
	}
	if _, e := call(t, srv, tokenFor("carol"), "close_item", map[string]any{"board": "alpha", "id": id}); !strings.Contains(e, "not allowed") {
		t.Errorf("carol closed bob's: %q", e)
	}
	if _, e := call(t, srv, bob, "close_item", map[string]any{"board": "alpha", "id": id, "note": "Done"}); e != "" {
		t.Errorf("close: %q", e)
	}

	// Removal takes effect on a token already issued.
	if err := st.DeleteMember(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, e := call(t, srv, bob, "list_items", map[string]any{"board": "alpha"}); !strings.Contains(e, "aren't on a board") {
		t.Errorf("removed member: %q", e)
	}
}

type bearerTransport struct {
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// A real MCP client, handshake included, works with an OAuth token.
func TestPersonToolsClient(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	const redirect = "http://localhost:4000/callback"
	client := registerClient(t, srv, redirect)
	access, _ := exchange(t, srv, client, redirect, getCode(t, h, srv, "bob", client, redirect))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{access}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_boards", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_boards: %v %+v", err, res)
	}
	tools, err := sess.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 12 {
		t.Fatalf("tools: %v %d", err, len(tools.Tools))
	}
}
