package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

type fake struct {
	srv   *httptest.Server
	posts map[string]map[string]any
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{posts: map[string]map[string]any{}}
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("PRIVATE-TOKEN") != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
				return
			}
			if r.Method == http.MethodPost {
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				f.posts[r.URL.EscapedPath()] = in
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = w.Write([]byte(body))
		}
	}
	// The project's path is one escaped segment, as GitLab wants it.
	const web = "/api/v4/projects/acme%2Fapps%2Fweb"
	route := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != method {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			h(w, r)
		})
	}
	route("GET", "/api/v4/user", reply(`{"username":"project_1_bot"}`))
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		switch key {
		case "GET " + web:
			reply(`{"path_with_namespace":"acme/apps/web","default_branch":"main","web_url":"https://gitlab.test/acme/apps/web",
				"visibility":"private","permissions":{"project_access":null,"group_access":{"access_level":30}}}`)(w, r)
		case "GET /api/v4/projects/acme%2Fdocs":
			reply(`{"path_with_namespace":"acme/docs","permissions":{"project_access":{"access_level":20}}}`)(w, r)
		case "GET " + web + "/issues":
			if r.URL.Query().Get("state") != "opened" {
				t.Errorf("state = %s", r.URL.Query().Get("state"))
			}
			reply(`[{"iid":7,"title":"Fix login","description":"long","state":"opened","labels":["bug"],"author":{"username":"ada"}}]`)(w, r)
		case "GET " + web + "/merge_requests/8":
			reply(`{"iid":8,"title":"MR","state":"opened","source_branch":"keera/fix"}`)(w, r)
		case "GET " + web + "/merge_requests/8/notes":
			reply(`[{"body":"changed the description","system":true},{"body":"Looks good","author":{"username":"bob"}}]`)(w, r)
		case "POST " + web + "/issues/7/notes", "POST " + web + "/merge_requests":
			reply(`{"iid":9,"title":"Draft: Fix login","state":"opened","web_url":"https://gitlab.test/acme/apps/web/-/merge_requests/9"}`)(w, r)
		default:
			t.Errorf("unexpected %s", key)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f, New(channels.Options{Endpoints: map[string]string{"base": f.srv.URL}})
}

func conn(token string) channels.Connection {
	return channels.Connection{Kind: channels.GitLab, Secrets: map[string]string{"token": token}}
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn("tok")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrong")); err == nil || !strings.Contains(err.Error(), "does not accept the token") {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestRepo(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	r, err := a.Repo(ctx, conn("tok"), "/acme/apps/web/")
	if err != nil || r.Path != "acme/apps/web" || r.DefaultBranch != "main" {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	if _, err := a.Repo(ctx, conn("tok"), "acme/docs"); err == nil || !strings.Contains(err.Error(), "Developer") {
		t.Fatalf("reporter token: %v", err)
	}
	for _, bad := range []string{"acme", "acme/../web", "acme/we b", "acme//web"} {
		if _, err := a.Repo(ctx, conn("tok"), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestRemote(t *testing.T) {
	_, a := newFake(t)
	self := conn("tok")
	self.Config = map[string]string{"base_url": "https://gitlab.example.com"}
	r, err := a.Remote(context.Background(), self, "acme/apps/web")
	if err != nil || r.URL != "https://gitlab.example.com/acme/apps/web.git" || r.User != "oauth2" || r.Password != "tok" {
		t.Fatalf("Remote = %+v, %v", r, err)
	}
}

func TestIssuesAndChanges(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	list, err := a.Issues(ctx, conn("tok"), "acme/apps/web", "open", false)
	if err != nil || len(list) != 1 || list[0].Number != 7 || list[0].State != "open" || list[0].Body != "" {
		t.Fatalf("Issues = %+v, %v", list, err)
	}
	mr, err := a.Issue(ctx, conn("tok"), "acme/apps/web", 8, true)
	if err != nil || !mr.Change || mr.Branch != "keera/fix" || len(mr.Comments) != 1 || mr.Comments[0].Author != "bob" {
		t.Fatalf("merge request = %+v, %v", mr, err)
	}
	if err := a.Comment(ctx, conn("tok"), "acme/apps/web", 7, false, "Fixed"); err != nil {
		t.Fatal(err)
	}
	out, err := a.OpenChange(ctx, conn("tok"), "acme/apps/web", channels.Change{
		Branch: "keera/fix-login", Base: "main", Title: "Fix login", Draft: true,
	})
	if err != nil || out.Number != 9 || !out.Change {
		t.Fatalf("OpenChange = %+v, %v", out, err)
	}
	sent := f.posts["/api/v4/projects/acme%2Fapps%2Fweb/merge_requests"]
	if sent["source_branch"] != "keera/fix-login" || sent["target_branch"] != "main" || sent["title"] != "Draft: Fix login" {
		t.Fatalf("sent %v", sent)
	}
}

// A GitLab that redirects to another server is not followed there: Go would
// send PRIVATE-TOKEN along. A redirect on the same server is followed.
func TestRedirectStaysOnTheServer(t *testing.T) {
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		_, _ = w.Write([]byte(`{"username":"project_1_bot"}`))
	}))
	defer elsewhere.Close()
	to := elsewhere.URL
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/moved/") {
			_, _ = w.Write([]byte(`{"username":"project_1_bot"}`))
			return
		}
		http.Redirect(w, r, to+r.URL.Path, http.StatusFound)
	}))
	defer gl.Close()

	a := New(channels.Options{Endpoints: map[string]string{"base": gl.URL}})
	if err := a.Check(context.Background(), conn("tok")); err == nil {
		t.Fatal("Check followed a redirect to another server")
	}
	if reached.Load() {
		t.Fatal("the other server was asked")
	}
	to = "/moved"
	if err := a.Check(context.Background(), conn("tok")); err != nil {
		t.Fatalf("a redirect on the same server: %v", err)
	}
}
