package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTenantAPI(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	prov := &fakeJob{}
	h.provisioner = prov
	call := func(token, method, path, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "hub.example"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}

	// Off without ADMIN_TOKEN.
	if code, _ := call("", "POST", "/api/tenants", `{"slug":"alpha"}`); code == 201 || code == 401 {
		t.Fatalf("routed without ADMIN_TOKEN: %d", code)
	}
	h.cfg.AdminToken = "adm"
	if code, _ := call("wrong", "POST", "/api/tenants", `{"slug":"alpha"}`); code != 401 {
		t.Fatalf("bad token: %d", code)
	}

	// An existing repo is ready at once and never provisioned.
	code, v := call("adm", "POST", "/api/tenants", `{"slug":"alpha","name":"Alpha","repo":"kirsch/alpha","existing":true}`)
	if code != 201 || v["status"] != "ready" || v["existing"] != true || v["board"] != "https://hub.example/alpha/" {
		t.Fatalf("existing: %d %v", code, v)
	}
	if len(prov.launches) != 0 {
		t.Fatalf("provisioned an existing repo: %v", prov.launches)
	}
	// A new app gets <TENANT_ORG>/<slug>, its owner as admin, and provisioning.
	code, v = call("adm", "POST", "/api/tenants", `{"slug":"beta","name":"Beta","owner":"kaylee@example.org"}`)
	if code != 201 || v["repo"] != "extreme-dot-org/beta" || len(prov.launches) != 1 {
		t.Fatalf("new: %d %v %v", code, v, prov.launches)
	}
	st, _ := h.reg.Store(ctx, "beta")
	if m, err := st.Member(ctx, "kaylee"); err != nil || m.Role != "admin" {
		t.Fatalf("owner: %+v %v", m, err)
	}
	// Taken slugs and bad JSON create nothing.
	if code, v := call("adm", "POST", "/api/tenants", `{"slug":"alpha","existing":true,"repo":"x/y"}`); code != 400 || v["error"] == "" {
		t.Errorf("duplicate: %d %v", code, v)
	}
	if code, _ := call("adm", "POST", "/api/tenants", `{`); code != 400 {
		t.Errorf("bad JSON: %d", code)
	}

	if code, v := call("adm", "GET", "/api/tenants/alpha", ""); code != 200 || v["repo"] != "kirsch/alpha" {
		t.Errorf("get: %d %v", code, v)
	}
	if code, _ := call("adm", "GET", "/api/tenants/nope", ""); code != 404 {
		t.Errorf("get missing: %d", code)
	}

	// Members: by login, or by email alone (login from the address).
	code, v = call("adm", "POST", "/api/tenants/alpha/members", `{"login":"@ZephyrLK","role":"member","email":"zephyr@example.org"}`)
	if code != 200 || v["login"] != "ZephyrLK" || v["github"] != false {
		t.Fatalf("member: %d %v", code, v)
	}
	st, _ = h.reg.Store(ctx, "alpha")
	if m, err := st.Member(ctx, "ZephyrLK"); err != nil || m.Role != "member" || m.Email != "zephyr@example.org" {
		t.Fatalf("stored member: %+v %v", m, err)
	}
	if code, v := call("adm", "POST", "/api/tenants/alpha/members", `{"email":"ann.b@example.org","role":"admin"}`); code != 200 || v["login"] != "annb" {
		t.Errorf("email-only member: %d %v", code, v)
	}
	if code, _ := call("adm", "POST", "/api/tenants/alpha/members", `{"login":"x","role":"owner"}`); code != 400 {
		t.Errorf("bad role: %d", code)
	}
	if code, _ := call("adm", "POST", "/api/tenants/alpha/members", `{"login":"y","role":"member","email":"zephyr@example.org"}`); code != 409 {
		t.Errorf("taken email: %d", code)
	}
	if code, _ := call("adm", "POST", "/api/tenants/nope/members", `{"login":"x","role":"member"}`); code != 404 {
		t.Errorf("missing tenant: %d", code)
	}

	job := &fakeJob{}
	h.launcher = job
	// Items: filed as the first ADMIN_LOGIN, approved when asked, tagged.
	code, v = call("adm", "POST", "/api/tenants/alpha/items", `{"title":"Group by hub #feature","body":"On the status page.","approve":true}`)
	if code != 201 || v["id"] != float64(1) || v["url"] != "https://hub.example/alpha/items/1" {
		t.Fatalf("file: %d %v", code, v)
	}
	if it, err := st.Item(ctx, 1); err != nil || it.Author != "nkirsch" || !it.Approved || it.ApprovedBy != "nkirsch" || len(it.Tags) != 1 {
		t.Errorf("filed item: %+v %v", it, err)
	}
	if code, _ := call("adm", "POST", "/api/tenants/alpha/items", `{"title":"Unapproved"}`); code != 201 {
		t.Errorf("file unapproved: %d", code)
	} else if it, _ := st.Item(ctx, 2); it.Approved {
		t.Errorf("approved without asking")
	}
	if code, v := call("adm", "POST", "/api/tenants/alpha/items", `{"title":"  "}`); code != 400 || v["error"] == "" {
		t.Errorf("no title: %d %v", code, v)
	}
	if code, _ := call("adm", "POST", "/api/tenants/nope/items", `{"title":"x"}`); code != 404 {
		t.Errorf("file on missing tenant: %d", code)
	}

	// Run now: launches the runner, then 409 while that run is active.
	if code, v := call("adm", "POST", "/api/tenants/alpha/run", ""); code != 201 || v["run"] != float64(1) || v["runs"] != "https://hub.example/alpha/runs" || len(job.launches) != 1 {
		t.Fatalf("run: %d %v %v", code, v, job.launches)
	}
	if code, v := call("adm", "POST", "/api/tenants/alpha/run", ""); code != 409 || v["error"] == "" {
		t.Errorf("second run: %d %v", code, v)
	}
}
