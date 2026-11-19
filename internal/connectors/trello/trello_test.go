package trello

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
)

const (
	boardID  = "5f0000000000000000000b01"
	otherID  = "5f0000000000000000000b02"
	todoID   = "5f0000000000000000000101"
	doneID   = "5f0000000000000000000102"
	elseList = "5f0000000000000000000201"
	cardID   = "5f0000000000000000000c01"
	strayID  = "5f0000000000000000000c02"
	labelID  = "5f0000000000000000000a01"
	memberID = "5f0000000000000000000d01"
	key      = "0123456789abcdef0123456789abcdef"
	token    = "ATTA0123456789abcdef"
)

type fake struct {
	// sent is each request's body, by method and path.
	sent map[string]map[string]any
	// query is each request's query, by method and path.
	query map[string]string
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]map[string]any{}, query: map[string]string{}}
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != `OAuth oauth_consumer_key="`+key+`", oauth_token="`+token+`"` {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("invalid token"))
				return
			}
			if strings.Contains(r.URL.RawQuery, token) {
				t.Errorf("%s: the token is in the query", r.URL.Path)
			}
			k := r.Method + " " + r.URL.Path
			f.query[k] = r.URL.RawQuery
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				var in map[string]any
				if err := json.Unmarshal(b, &in); err != nil {
					t.Errorf("%s: %v", k, err)
				}
				f.sent[k] = in
			}
			_, _ = w.Write([]byte(body))
		}
	}
	board := `{"id":"` + boardID + `","name":"Roadmap","desc":"Plans","url":"https://trello.com/b/AbC123xY/roadmap"}`
	mux.HandleFunc("GET /1/members/me", reply(`{"id":"`+memberID+`","username":"agents"}`))
	mux.HandleFunc("GET /1/members/me/boards", reply(`[`+board+`,{"id":"`+otherID+`","name":"Other","url":"https://trello.com/b/ZzZ999zz/other"}]`))
	mux.HandleFunc("GET /1/boards/AbC123xY", reply(board))
	mux.HandleFunc("GET /1/boards/"+boardID, reply(`{"id":"`+boardID+`","name":"Roadmap","url":"https://trello.com/b/AbC123xY/roadmap",
		"lists":[{"id":"`+todoID+`","name":"To do"},{"id":"`+doneID+`","name":"Done"}],
		"labels":[{"id":"`+labelID+`","name":"","color":"red"}],
		"members":[{"id":"`+memberID+`","fullName":"Ada Lovelace","username":"ada"}],
		"cards":[
			{"id":"`+cardID+`","name":"Ship it","idBoard":"`+boardID+`","idList":"`+todoID+`","idMembers":["`+memberID+`"],
			 "labels":[{"id":"`+labelID+`","color":"red"}],"due":null,"shortUrl":"https://trello.com/c/CaRd0001"},
			{"id":"5f0000000000000000000c03","name":"Shipped","idBoard":"`+boardID+`","idList":"`+doneID+`","shortUrl":"https://trello.com/c/CaRd0003"}]}`))
	mux.HandleFunc("GET /1/boards/NoPe0000", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("The requested resource was not found."))
	})
	mux.HandleFunc("GET /1/lists/"+todoID, reply(`{"id":"`+todoID+`","idBoard":"`+boardID+`"}`))
	mux.HandleFunc("GET /1/lists/"+doneID, reply(`{"id":"`+doneID+`","idBoard":"`+boardID+`"}`))
	mux.HandleFunc("GET /1/lists/"+elseList, reply(`{"id":"`+elseList+`","idBoard":"`+otherID+`"}`))
	mux.HandleFunc("GET /1/cards/"+cardID, reply(`{"id":"`+cardID+`","name":"Ship it","desc":"`+strings.Repeat("x", maxDescription+10)+`",
		"idBoard":"`+boardID+`","idList":"`+todoID+`","idMembers":["`+memberID+`"],"due":"2026-12-01T17:00:00.000Z",
		"shortUrl":"https://trello.com/c/CaRd0001","list":{"name":"To do"},
		"members":[{"id":"`+memberID+`","fullName":"Ada Lovelace"}],
		"checklists":[{"name":"Steps","checkItems":[{"name":"Build","state":"complete"},{"name":"Test","state":"incomplete"}]}],
		"actions":[{"date":"2026-11-02","memberCreator":{"fullName":"Bob"},"data":{"text":"second"}},
		           {"date":"2026-11-01","memberCreator":{"fullName":"Ada Lovelace"},"data":{"text":"first"}}]}`))
	mux.HandleFunc("GET /1/cards/CaRd0001", reply(`{"id":"`+cardID+`","idBoard":"`+boardID+`","idList":"`+todoID+`","shortUrl":"https://trello.com/c/CaRd0001"}`))
	mux.HandleFunc("GET /1/cards/"+strayID, reply(`{"id":"`+strayID+`","idBoard":"`+otherID+`","idList":"`+elseList+`"}`))
	mux.HandleFunc("GET /1/search", reply(`{"cards":[
		{"id":"`+cardID+`","name":"Ship it","idBoard":"`+boardID+`","idList":"`+todoID+`","list":{"name":"To do"}},
		{"id":"`+strayID+`","name":"Elsewhere","idBoard":"`+otherID+`","idList":"`+elseList+`"}]}`))
	mux.HandleFunc("POST /1/cards", reply(`{"id":"5f0000000000000000000c09","name":"New","idBoard":"`+boardID+`","idList":"`+todoID+`"}`))
	mux.HandleFunc("PUT /1/cards/"+cardID, reply(`{"id":"`+cardID+`","name":"Ship it","idList":"`+doneID+`"}`))
	mux.HandleFunc("POST /1/cards/"+cardID+"/actions/comments", reply(`{"id":"x"}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, New(connectors.Options{Endpoints: map[string]string{"api": srv.URL}})
}

func conn(tok string) connectors.Connection {
	return connectors.Connection{
		Kind: connectors.Trello, Config: map[string]string{"api_key": key}, Secrets: map[string]string{"token": tok},
	}
}

func call(t *testing.T, a *Adapter, target, tool, args string) any {
	t.Helper()
	out, err := a.Call(context.Background(), conn(token), target, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out
}

func callErr(t *testing.T, a *Adapter, target, tool, args string) error {
	t.Helper()
	_, err := a.Call(context.Background(), conn(token), target, tool, json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s %s: no error", tool, args)
	}
	return err
}

func TestDescriptor(t *testing.T) {
	d := New(connectors.Options{}).Descriptor()
	if d.Kind != connectors.Trello || d.Vendor != "Atlassian" || d.TargetLabel == "" || !d.AnyTarget {
		t.Fatalf("descriptor = %+v", d)
	}
	write := map[string]bool{}
	for _, tool := range New(connectors.Options{}).Tools() {
		write[tool.Name] = tool.Write
	}
	for _, name := range []string{"list_boards", "get_board", "list_cards", "search_cards", "get_card"} {
		if w, ok := write[name]; !ok || w {
			t.Errorf("%s: write %v, present %v", name, w, ok)
		}
	}
	for _, name := range []string{"create_card", "update_card", "comment"} {
		if !write[name] {
			t.Errorf("%s does not write", name)
		}
	}
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn(token)); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrongtoken0123456789")); err == nil || !strings.Contains(err.Error(), "do not work") {
		t.Fatalf("wrong token: %v", err)
	}
	if err := a.Check(ctx, conn(`bad", oauth_x="y`)); err == nil || !strings.Contains(err.Error(), "letters and digits") {
		t.Fatalf("token that breaks the header: %v", err)
	}
	if err := a.Check(ctx, conn("")); err == nil {
		t.Fatal("no token passed")
	}
}

func TestRef(t *testing.T) {
	for in, want := range map[string]string{
		"AbC123xY":                               "AbC123xY",
		" https://trello.com/b/AbC123xY/roadmap": "AbC123xY",
		"https://trello.com/b/AbC123xY":          "AbC123xY",
		boardID:                                  boardID,
		"https://trello.com/c/AbC123xY/1-card":   "",
		"../../1/members":                        "",
		"AbC123x":                                "",
	} {
		got, ok := ref(in, "b")
		if got != want || ok != (want != "") {
			t.Errorf("ref(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestTarget(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	got, err := a.Target(ctx, conn(token), "https://trello.com/b/AbC123xY/roadmap")
	if err != nil || got != boardID {
		t.Fatalf("target = %q, %v", got, err)
	}
	if got, _ := a.Target(ctx, conn(token), connectors.AnyTarget); got != connectors.AnyTarget {
		t.Fatalf("any = %q", got)
	}
	if _, err := a.Target(ctx, conn(token), "NoPe0000"); err == nil || !strings.Contains(err.Error(), "cannot see") {
		t.Fatalf("unknown board: %v", err)
	}
	if _, err := a.Target(ctx, conn(token), "not a board"); err == nil {
		t.Fatal("not a board passed")
	}
}

func TestBoards(t *testing.T) {
	_, a := newFake(t)
	if bs := call(t, a, connectors.AnyTarget, "list_boards", `{}`).([]Board); len(bs) != 2 {
		t.Fatalf("every board = %+v", bs)
	}
	if bs := call(t, a, boardID, "list_boards", `{}`).([]Board); len(bs) != 1 || bs[0].Name != "Roadmap" {
		t.Fatalf("the binding's board = %+v", bs)
	}
	d := call(t, a, boardID, "get_board", `{}`).(BoardDetail)
	if len(d.Lists) != 2 || d.Labels[0].Color != "red" || d.Members[0].Name != "Ada Lovelace" {
		t.Fatalf("board = %+v", d)
	}
	if err := callErr(t, a, connectors.AnyTarget, "get_board", `{}`); !strings.Contains(err.Error(), "give board") {
		t.Fatalf("no board: %v", err)
	}
	if err := callErr(t, a, otherID, "get_board", `{"board":"AbC123xY"}`); !strings.Contains(err.Error(), "only its own board") {
		t.Fatalf("another board: %v", err)
	}
}

func TestListCards(t *testing.T) {
	_, a := newFake(t)
	out := call(t, a, boardID, "list_cards", `{"list":"`+todoID+`"}`).(map[string]any)
	cs := out["cards"].([]Brief)
	if len(cs) != 1 || cs[0].List != "To do" || cs[0].Labels[0] != "red" || cs[0].Members[0] != "Ada Lovelace" {
		t.Fatalf("cards = %+v", cs)
	}
	out = call(t, a, boardID, "list_cards", `{"limit":1}`).(map[string]any)
	if len(out["cards"].([]Brief)) != 1 || out["more"] != true {
		t.Fatalf("limited = %+v", out)
	}
	if err := callErr(t, a, boardID, "list_cards", `{"list":"`+elseList+`"}`); !strings.Contains(err.Error(), "not on this connector's board") {
		t.Fatalf("list elsewhere: %v", err)
	}
}

func TestSearchCards(t *testing.T) {
	f, a := newFake(t)
	cs := call(t, a, boardID, "search_cards", `{"query":"ship"}`).(map[string]any)["cards"].([]Brief)
	if len(cs) != 1 || cs[0].ID != cardID || cs[0].List != "To do" {
		t.Fatalf("cards = %+v", cs)
	}
	if q := f.query["GET /1/search"]; !strings.Contains(q, "idBoards="+boardID) {
		t.Fatalf("query = %s", q)
	}
	cs = call(t, a, connectors.AnyTarget, "search_cards", `{"query":"ship"}`).(map[string]any)["cards"].([]Brief)
	if len(cs) != 2 || !strings.Contains(f.query["GET /1/search"], "idBoards=mine") {
		t.Fatalf("every board: %+v, %s", cs, f.query["GET /1/search"])
	}
}

func TestGetCard(t *testing.T) {
	_, a := newFake(t)
	c := call(t, a, boardID, "get_card", `{"card":"`+cardID+`"}`).(Card)
	if c.List != "To do" || c.Members[0] != "Ada Lovelace" || c.Due == "" || len(c.Description) > maxDescription+10 {
		t.Fatalf("card = %+v", c.Brief)
	}
	if c.Checklists[0].Items[0] != "[x] Build" || c.Checklists[0].Items[1] != "[ ] Test" {
		t.Fatalf("checklists = %+v", c.Checklists)
	}
	if c.Comments[0].Text != "first" || c.Comments[1].Author != "Bob" {
		t.Fatalf("comments = %+v", c.Comments)
	}
	if err := callErr(t, a, boardID, "get_card", `{"card":"`+strayID+`"}`); !strings.Contains(err.Error(), "not on this connector's board") {
		t.Fatalf("card elsewhere: %v", err)
	}
	if err := callErr(t, a, boardID, "get_card", `{"card":"../members/me"}`); !strings.Contains(err.Error(), "not a card") {
		t.Fatalf("bad card: %v", err)
	}
}

func TestCreateCard(t *testing.T) {
	f, a := newFake(t)
	call(t, a, boardID, "create_card", `{"list":"`+todoID+`","name":" New ","labels":["`+labelID+`"],"top":true}`)
	sent := f.sent["POST /1/cards"]
	if sent["idList"] != todoID || sent["name"] != "New" || sent["idLabels"] != labelID || sent["pos"] != "top" {
		t.Fatalf("sent = %v", sent)
	}
	if err := callErr(t, a, boardID, "create_card", `{"list":"`+elseList+`","name":"x"}`); !strings.Contains(err.Error(), "not on this connector's board") {
		t.Fatalf("list elsewhere: %v", err)
	}
	if err := callErr(t, a, boardID, "create_card", `{"list":"`+todoID+`","name":"x","labels":["red"]}`); !strings.Contains(err.Error(), "label id") {
		t.Fatalf("bad label: %v", err)
	}
}

func TestUpdateCard(t *testing.T) {
	f, a := newFake(t)
	call(t, a, boardID, "update_card", `{"card":"https://trello.com/c/CaRd0001/1-ship-it","list":"`+doneID+`","due":"none","done":true,"labels":[]}`)
	sent := f.sent["PUT /1/cards/"+cardID]
	if sent["idList"] != doneID || sent["dueComplete"] != true || sent["idLabels"] != "" {
		t.Fatalf("sent = %v", sent)
	}
	if v, ok := sent["due"]; !ok || v != nil {
		t.Fatalf("due = %v, %v", v, ok)
	}
	if _, ok := sent["name"]; ok {
		t.Fatalf("name sent: %v", sent)
	}
	// Even a connector to every board moves a card only within its board.
	if err := callErr(t, a, connectors.AnyTarget, "update_card", `{"card":"`+cardID+`","list":"`+elseList+`"}`); !strings.Contains(err.Error(), "another board") {
		t.Fatalf("move elsewhere: %v", err)
	}
	if err := callErr(t, a, boardID, "update_card", `{"card":"`+cardID+`"}`); !strings.Contains(err.Error(), "something to change") {
		t.Fatalf("nothing: %v", err)
	}
	if err := callErr(t, a, boardID, "update_card", `{"card":"`+strayID+`","name":"x"}`); !strings.Contains(err.Error(), "not on this connector's board") {
		t.Fatalf("card elsewhere: %v", err)
	}
}

func TestComment(t *testing.T) {
	f, a := newFake(t)
	call(t, a, boardID, "comment", `{"card":"`+cardID+`","text":"Done."}`)
	if f.sent["POST /1/cards/"+cardID+"/actions/comments"]["text"] != "Done." {
		t.Fatalf("sent = %v", f.sent)
	}
	if err := callErr(t, a, boardID, "comment", `{"card":"`+strayID+`","text":"x"}`); !strings.Contains(err.Error(), "not on this connector's board") {
		t.Fatalf("card elsewhere: %v", err)
	}
}
