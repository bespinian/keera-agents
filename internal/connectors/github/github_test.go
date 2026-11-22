package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
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
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			if r.Method == http.MethodPost {
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				f.posts[r.URL.Path] = in
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET /api/user", reply(`{"login":"keera-bot"}`))
	mux.HandleFunc("GET /api/repos/acme/web", reply(
		`{"full_name":"acme/web","default_branch":"main","html_url":"https://github.com/acme/web","private":true,"permissions":{"push":true}}`))
	mux.HandleFunc("GET /api/repos/acme/docs", reply(`{"full_name":"acme/docs","permissions":{"push":false}}`))
	mux.HandleFunc("GET /api/repos/acme/web/issues", reply(`[
		{"number":2,"title":"Fix login","body":"long","state":"open","user":{"login":"ada"},"labels":[{"name":"bug"}]},
		{"number":3,"title":"A pull request","state":"open","pull_request":{}}]`))
	mux.HandleFunc("GET /api/repos/acme/web/issues/2", reply(`{"number":2,"title":"Fix login","body":"It fails.","state":"open"}`))
	mux.HandleFunc("GET /api/repos/acme/web/issues/2/comments", reply(`[{"body":"Me too","user":{"login":"bob"}}]`))
	mux.HandleFunc("GET /api/repos/acme/web/pulls/3", reply(`{"number":3,"title":"PR","state":"open","head":{"ref":"keera/fix"}}`))
	mux.HandleFunc("GET /api/repos/acme/web/issues/3/comments", reply(`[]`))
	mux.HandleFunc("POST /api/repos/acme/web/issues/2/comments", reply(`{}`))
	mux.HandleFunc("POST /api/repos/acme/web/pulls", reply(
		`{"number":4,"title":"Fix login","state":"open","html_url":"https://github.com/acme/web/pull/4","head":{"ref":"keera/fix-login"}}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	a := New(connectors.Options{Endpoints: map[string]string{"api": f.srv.URL + "/api", "git": "https://git.test"}})
	return f, a
}

func conn(token string) connectors.Connection {
	return connectors.Connection{Kind: connectors.GitHub, Secrets: map[string]string{"token": token}}
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
	if err := a.Check(ctx, conn("")); err == nil {
		t.Fatal("no token passed")
	}
	bad := conn("tok")
	bad.Config = map[string]string{"base_url": "http://github.example.com"}
	if err := a.Check(ctx, bad); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain http enterprise URL: %v", err)
	}
}

func TestRepo(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	r, err := a.Repo(ctx, conn("tok"), " acme/web.git ")
	if err != nil || r.Path != "acme/web" || r.DefaultBranch != "main" {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	if _, err := a.Repo(ctx, conn("tok"), "acme/docs"); err == nil || !strings.Contains(err.Error(), "not push") {
		t.Fatalf("read-only repo: %v", err)
	}
	for _, bad := range []string{"acme", "acme/web/extra", "../etc", "acme/..", "acme/we b"} {
		if _, err := a.Repo(ctx, conn("tok"), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestRemote(t *testing.T) {
	_, a := newFake(t)
	r, err := a.Remote(context.Background(), conn("tok"), "acme/web")
	if err != nil || r.URL != "https://git.test/acme/web.git" || r.User != "x-access-token" || r.Password != "tok" {
		t.Fatalf("Remote = %+v, %v", r, err)
	}
	enterprise := conn("tok")
	enterprise.Config = map[string]string{"base_url": "https://github.example.com/"}
	if r, _ := a.Remote(context.Background(), enterprise, "acme/web"); r.URL != "https://github.example.com/acme/web.git" {
		t.Fatalf("enterprise remote = %s", r.URL)
	}
}

func TestIssues(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	list, err := a.Issues(ctx, conn("tok"), "acme/web", "open", false)
	if err != nil || len(list) != 1 || list[0].Number != 2 || list[0].Body != "" || list[0].Labels[0] != "bug" {
		t.Fatalf("Issues = %+v, %v", list, err)
	}
	i, err := a.Issue(ctx, conn("tok"), "acme/web", 2, false)
	if err != nil || i.Body != "It fails." || len(i.Comments) != 1 || i.Comments[0].Author != "bob" {
		t.Fatalf("Issue = %+v, %v", i, err)
	}
	pr, err := a.Issue(ctx, conn("tok"), "acme/web", 3, true)
	if err != nil || !pr.Change || pr.Branch != "keera/fix" {
		t.Fatalf("pull request = %+v, %v", pr, err)
	}
	if err := a.Comment(ctx, conn("tok"), "acme/web", 2, false, "Fixed"); err != nil {
		t.Fatal(err)
	}
	if got := f.posts["/api/repos/acme/web/issues/2/comments"]["body"]; got != "Fixed" {
		t.Fatalf("comment body = %v", got)
	}
}

func TestOpenChange(t *testing.T) {
	f, a := newFake(t)
	out, err := a.OpenChange(context.Background(), conn("tok"), "acme/web", connectors.Change{
		Branch: "keera/fix-login", Base: "main", Title: "Fix login", Body: "Closes #2", Draft: true,
	})
	if err != nil || out.Number != 4 || !out.Change || out.WebURL == "" {
		t.Fatalf("OpenChange = %+v, %v", out, err)
	}
	sent := f.posts["/api/repos/acme/web/pulls"]
	if sent["head"] != "keera/fix-login" || sent["base"] != "main" || sent["draft"] != true || sent["body"] != "Closes #2" {
		t.Fatalf("sent %v", sent)
	}
}

// A person's token is used for the API and for git, and errors name their
// account.
func TestPerUser(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	user := connectors.Connection{
		Kind: connectors.GitHub, PerUser: true, Token: "tok", Account: "ada",
		Config: map[string]string{"client_id": "Iv1.x"}, Secrets: map[string]string{"client_secret": "pw"},
	}
	if r, err := a.Repo(ctx, user, "acme/web"); err != nil || r.DefaultBranch != "main" {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	if _, err := a.Repo(ctx, user, "acme/docs"); err == nil || !strings.Contains(err.Error(), "ada may read") {
		t.Errorf("no push: %v", err)
	}
	if r, err := a.Remote(ctx, user, "acme/web"); err != nil || r.Password != "tok" {
		t.Errorf("Remote = %+v, %v", r, err)
	}
	user.Config["base_url"] = "https://ghe.example"
	u, err := a.SignInURL(user, "https://k/cb", "st", "ch")
	if err != nil || !strings.HasPrefix(u, "https://ghe.example/login/oauth/authorize?") {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
	user.Token = ""
	if _, err := a.Remote(ctx, user, "acme/web"); err == nil {
		t.Error("a remote for nobody")
	}
}

// A busy repository's newest items can all be pull requests, and a long
// thread's newest comments are on its last page.
func TestIssuesBusy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/acme/busy/issues", func(w http.ResponseWriter, r *http.Request) {
		var items []string
		if r.URL.Query().Get("page") == "1" {
			for i := range 100 {
				items = append(items, fmt.Sprintf(`{"number":%d,"pull_request":{}}`, 1000+i))
			}
		} else {
			items = append(items, `{"number":7,"title":"Old bug","state":"open"}`, `{"number":8,"pull_request":{}}`)
		}
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	})
	mux.HandleFunc("GET /api/repos/acme/busy/issues/7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"title":"Old bug","state":"open","comments":150}`))
	})
	mux.HandleFunc("GET /api/repos/acme/busy/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		first := map[string]int{"1": 1, "2": 101}[r.URL.Query().Get("page")]
		var items []string
		for n := first; first > 0 && n < first+100 && n <= 150; n++ {
			items = append(items, fmt.Sprintf(`{"body":"c%d"}`, n))
		}
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := New(connectors.Options{Endpoints: map[string]string{"api": srv.URL + "/api"}})
	ctx := context.Background()
	list, err := a.Issues(ctx, conn("tok"), "acme/busy", "open", false)
	if err != nil || len(list) != 1 || list[0].Number != 7 {
		t.Fatalf("Issues = %+v, %v", list, err)
	}
	i, err := a.Issue(ctx, conn("tok"), "acme/busy", 7, false)
	if err != nil || len(i.Comments) != maxComments || i.Comments[0].Body != "c51" || i.Comments[99].Body != "c150" {
		t.Fatalf("Issue = %d comments, %v", len(i.Comments), err)
	}
}
