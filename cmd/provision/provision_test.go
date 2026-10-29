package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// fakeShell records commands; describes fail until exists is set.
type fakeShell struct {
	cmds   []string
	envs   [][]string
	exists bool
	cname  string // the CNAME's current target once it exists
}

func (f *fakeShell) Run(_ context.Context, env []string, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	f.cmds, f.envs = append(f.cmds, cmd), append(f.envs, env)
	if strings.Contains(cmd, "projects describe") {
		return "123\n", nil
	}
	if strings.Contains(cmd, " describe ") && !f.exists {
		return "", errors.New("NOT_FOUND")
	}
	if strings.Contains(cmd, "rrdatas)") {
		return f.cname + "\n", nil
	}
	return "", nil
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	vars := map[string]string{}
	repoMade := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/extreme-dot-org/alpha":
			if !repoMade {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{}`))
		case "GET /repos/extreme-dot-org/alpha/commits":
			w.Write([]byte(`[{}]`))
		case "POST /orgs/extreme-dot-org/repos":
			repoMade = true
			w.WriteHeader(http.StatusCreated)
		case "POST /repos/extreme-dot-org/alpha/actions/variables":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			vars[in["name"]] = in["value"]
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sh := &fakeShell{}
	p := &Provisioner{Project: "proj", Region: "us-central1", Domain: "extreme.org", Zone: "extreme-org",
		Pool: "extreme", Provider: "github", Template: "extreme-dot-org/extreme-template",
		GitHub: github.New(srv.URL, "t"), Shell: sh,
		Token: func(context.Context) (string, error) { return "sekrit", nil }}
	var reports []string
	p.Report = func(_ context.Context, step, state, errText string) {
		reports = append(reports, step+" "+state+errText)
	}
	tn := tenant.Tenant{Slug: "alpha", Name: "Alpha", Repo: "extreme-dot-org/alpha"}
	if err := p.Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	// Each step the job covers is reported running, then done, in order.
	var want []string
	for _, step := range tenant.ProvisionSteps[2 : len(tenant.ProvisionSteps)-1] {
		want = append(want, step+" running", step+" done")
	}
	if !slices.Equal(reports, want) {
		t.Errorf("reports %q, want %q", reports, want)
	}
	all := strings.Join(sh.cmds, "\n")
	for _, want := range []string{
		"gcloud --project proj --quiet iam service-accounts create app-alpha --display-name alpha app",
		"iam service-accounts create deploy-alpha",
		"artifacts repositories create x-alpha --location us-central1 --repository-format docker",
		"run deploy x-alpha --region us-central1 --image us-docker.pkg.dev/cloudrun/container/hello --service-account app-alpha@proj.iam.gserviceaccount.com --allow-unauthenticated",
		"beta run domain-mappings create --service x-alpha --domain alpha.extreme.org",
		`logging views create alpha-requests --bucket _Default --location global --log-filter resource.type="cloud_run_revision" AND resource.labels.service_name="x-alpha"`,
		"dns record-sets create alpha.extreme.org. --type CNAME --ttl 300 --rrdatas ghs.googlehosted.com. --zone extreme-org",
		"--member principalSet://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/extreme/attribute.repository/extreme-dot-org/alpha --role roles/iam.workloadIdentityUser",
		"git clone --quiet --branch master https://github.com/extreme-dot-org/extreme-template.git",
		"https://github.com/extreme-dot-org/alpha.git HEAD:refs/heads/master",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "sekrit") {
		t.Error("token in argv")
	}
	if !slices.ContainsFunc(sh.envs[len(sh.envs)-1], func(e string) bool {
		return strings.HasPrefix(e, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic ")
	}) {
		t.Error("template copy without the auth header")
	}
	if vars["SERVICE"] != "x-alpha" || vars["IMAGE"] != "us-central1-docker.pkg.dev/proj/x-alpha/app" ||
		vars["GCP_DEPLOYER"] != "deploy-alpha@proj.iam.gserviceaccount.com" ||
		vars["GCP_WIF_PROVIDER"] != "projects/123/locations/global/workloadIdentityPools/extreme/providers/github" {
		t.Errorf("vars %v", vars)
	}

	// A sync finds everything in place: no creates and no second template copy.
	sh.cmds, sh.exists, sh.cname = nil, true, "ghs.googlehosted.com."
	if err := p.Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	for _, c := range sh.cmds {
		if strings.Contains(c, " create ") || strings.Contains(c, " deploy ") || strings.HasPrefix(c, "git ") {
			t.Errorf("sync ran %q", c)
		}
	}

	// A CNAME pointing elsewhere (www's old static site) is replaced.
	sh.cmds, sh.cname = nil, "www.extreme.org.s3-website-us-east-1.amazonaws.com."
	if err := p.Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(sh.cmds, "\n"), "dns record-sets update alpha.extreme.org. --type CNAME --ttl 300 --rrdatas ghs.googlehosted.com. --zone extreme-org") {
		t.Errorf("stale CNAME kept:\n%s", strings.Join(sh.cmds, "\n"))
	}
}

func TestDelete(t *testing.T) {
	archived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == "PATCH /repos/extreme-dot-org/alpha" {
			archived = true
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	sh := &goneShell{}
	p := &Provisioner{Project: "proj", Region: "us-central1", Domain: "extreme.org", Zone: "extreme-org",
		GitHub: github.New(srv.URL, "t"), Shell: sh}
	if err := p.Delete(context.Background(), tenant.Tenant{Slug: "alpha", Repo: "extreme-dot-org/alpha"}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(sh.cmds, "\n")
	for _, want := range []string{"domain-mappings delete --domain alpha.extreme.org", "run services delete x-alpha",
		"artifacts repositories delete x-alpha", "logging views delete alpha-requests", "service-accounts delete app-alpha@", "service-accounts delete deploy-alpha@"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if !archived {
		t.Error("repo not archived")
	}
}

// goneShell reports the DNS record as already deleted.
type goneShell struct{ cmds []string }

func (g *goneShell) Run(_ context.Context, _ []string, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	g.cmds = append(g.cmds, cmd)
	if strings.Contains(cmd, "record-sets delete") {
		return "", errors.New("HTTPError 404: does not exist")
	}
	return "", nil
}

// A failing step is reported failed with its error, and later steps don't run.
func TestCreateReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{}]`)) // the repo exists and has commits
	}))
	defer srv.Close()
	sh := &failShell{fakeShell: fakeShell{exists: false}, fail: "domain-mappings create"}
	reports := map[string]string{}
	p := &Provisioner{Project: "proj", Region: "us-central1", Domain: "extreme.org", Zone: "extreme-org",
		GitHub: github.New(srv.URL, "t"), Shell: sh,
		Report: func(_ context.Context, step, state, errText string) { reports[step] = state + " " + errText }}
	if err := p.Create(context.Background(), tenant.Tenant{Slug: "alpha", Repo: "extreme-dot-org/alpha"}); err == nil {
		t.Fatal("no error")
	}
	if got := reports["Domain mapping"]; !strings.HasPrefix(got, "failed beta run: ") || !strings.Contains(got, "not verified") {
		t.Errorf("domain mapping %q", got)
	}
	if reports["Image repo and service"] != "done " || reports["DNS record"] != "" {
		t.Errorf("reports %v", reports)
	}
}

// failShell is a fakeShell whose commands containing fail error out.
type failShell struct {
	fakeShell
	fail string
}

func (f *failShell) Run(ctx context.Context, env []string, args ...string) (string, error) {
	if strings.Contains(strings.Join(args, " "), f.fail) {
		return "", errors.New("domain not verified")
	}
	return f.fakeShell.Run(ctx, env, args...)
}
