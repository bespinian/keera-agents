package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace/workspacetest"
)

const users = "/gmail/v1/users/me"

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// setup fakes a mailbox with a plain and HTML message, an HTML-only one, and
// records what is sent.
func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]map[string]any) {
	f := workspacetest.New(t)
	var posted []map[string]any
	f.Handle("GET "+users+"/profile", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"emailAddress":"ada@acme.com"}`))
	})
	f.Handle("GET "+users+"/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "in:inbox" {
			t.Errorf("q = %s", r.URL.Query().Get("q"))
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"m1"},{"id":"m2"}],"resultSizeEstimate":2}`))
	})
	m1 := map[string]any{
		"id": "m1", "threadId": "t1", "labelIds": []string{"INBOX", "UNREAD"}, "snippet": "Please pay &amp; thanks",
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers": []map[string]string{
				{"name": "From", "value": "Bob <bob@example.com>"},
				{"name": "To", "value": "ada@acme.com, Cy <cy@example.com>"},
				{"name": "Cc", "value": "dee@example.com"},
				{"name": "Subject", "value": "Invoice"},
				{"name": "Date", "value": "Tue, 3 Nov 2026 09:00:00 +0100"},
				{"name": "Message-ID", "value": "<m1@example.com>"},
			},
			"parts": []map[string]any{
				{"mimeType": "multipart/alternative", "parts": []map[string]any{
					{"mimeType": "text/plain", "body": map[string]any{"data": b64("Hello\r\n\r\n\r\n\r\nPlease pay.")}},
					{"mimeType": "text/html", "body": map[string]any{"data": b64("<p>Hello</p>")}},
				}},
				{"mimeType": "application/pdf", "filename": "invoice.pdf", "body": map[string]any{"size": 1234, "attachmentId": "a1"}},
			},
		},
	}
	m2 := map[string]any{
		"id": "m2", "threadId": "t2", "labelIds": []string{"INBOX"},
		"payload": map[string]any{
			"mimeType": "text/html", "headers": []map[string]string{{"name": "Subject", "value": "News"}},
			"body": map[string]any{"data": b64(`<style>p{}</style><h1>Big &amp; news</h1><ul><li>one</li><li>two</li></ul>`)},
		},
	}
	f.Handle("GET "+users+"/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "m1":
			_ = json.NewEncoder(w).Encode(m1)
		case "m2":
			_ = json.NewEncoder(w).Encode(m2)
		default:
			workspacetest.Error(w, http.StatusNotFound, "Requested entity was not found.")
		}
	})
	f.Handle("GET "+users+"/threads/t1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{m1}})
	})
	f.Handle("GET "+users+"/labels", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"UNREAD","name":"UNREAD","type":"system"},
			{"id":"Label_7","name":"Invoices","type":"user"}]}`))
	})
	record := func(reply string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			in := workspacetest.JSON(r)
			in["path"] = r.URL.Path
			posted = append(posted, in)
			_, _ = w.Write([]byte(reply))
		}
	}
	f.Handle("POST "+users+"/messages/send", record(`{"id":"s1","threadId":"t1"}`))
	f.Handle("POST "+users+"/drafts", record(`{"id":"d1","message":{"id":"s2","threadId":"t1"}}`))
	f.Handle("POST "+users+"/messages/m1/modify", record(`{"id":"m1","labelIds":["Label_7"]}`))
	return f, New(f.Options()), &posted
}

func shared(f *workspacetest.Fake) connectors.Connection {
	return f.Conn(connectors.Gmail, "")
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// sentMessage decodes the raw message of a send or draft.
func sentMessage(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, _ := payload["raw"].(string)
	if m, ok := payload["message"].(map[string]any); ok {
		raw, _ = m["raw"].(string)
	}
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	return string(b)
}

func TestTarget(t *testing.T) {
	f, a, _ := setup(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, shared(f), " Ada@Acme.com "); err != nil || got != "ada@acme.com" {
		t.Errorf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, shared(f), workspacetest.Outsider); err == nil || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Errorf("a mailbox without delegation: %v", err)
	}
	for _, bad := range []string{"*", "me", "Ada <ada@acme.com>", "not an address"} {
		if _, err := a.Target(ctx, shared(f), bad); err == nil {
			t.Errorf("Target(%q) passed", bad)
		}
	}
	// A check that could not run is no answer, so the mailbox is refused.
	broken := shared(f)
	broken.Secrets = map[string]string{"service_account_json": "{}"}
	if _, err := a.Target(ctx, broken, "ada@acme.com"); err == nil {
		t.Error("a bad key passed")
	}
	user := f.UserConn(connectors.Gmail, "ws-token")
	if got, err := a.Target(ctx, user, "me"); err != nil || got != "me" {
		t.Errorf("Target(me) = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, user, "boss@acme.com"); err == nil {
		t.Error("a person's sign-in reached another mailbox")
	}
}

func TestRead(t *testing.T) {
	f, a, _ := setup(t)
	out, err := call(t, a, shared(f), "ada@acme.com", "search_messages", `{}`)
	if err != nil || !strings.Contains(out, `"preview":"Please pay \u0026 thanks"`) || !strings.Contains(out, `"unread":true`) ||
		!strings.Contains(out, `"subject":"News"`) {
		t.Errorf("search = %s, %v", out, err)
	}
	out, err = call(t, a, shared(f), "ada@acme.com", "read_message", `{"id":"m1"}`)
	if err != nil || !strings.Contains(out, `"body":"Hello\n\nPlease pay."`) || !strings.Contains(out, `"name":"invoice.pdf"`) ||
		!strings.Contains(out, `"cc":"dee@example.com"`) {
		t.Errorf("read_message = %s, %v", out, err)
	}
	// Without a plain part, the HTML is read as text.
	out, err = call(t, a, shared(f), "ada@acme.com", "read_message", `{"id":"m2"}`)
	if err != nil || !strings.Contains(out, `"body":"Big \u0026 news\n- one\n- two"`) {
		t.Errorf("an HTML message = %s, %v", out, err)
	}
	if _, err := call(t, a, shared(f), "ada@acme.com", "read_message", `{"id":"../labels"}`); err == nil {
		t.Error("a path as an id passed")
	}
	out, err = call(t, a, shared(f), "ada@acme.com", "read_thread", `{"thread_id":"t1"}`)
	if err != nil || !strings.Contains(out, `"subject":"Invoice"`) {
		t.Errorf("read_thread = %s, %v", out, err)
	}
}

func TestSend(t *testing.T) {
	f, a, posted := setup(t)
	box := "ada@acme.com"
	_, err := call(t, a, shared(f), box, "send_message",
		`{"to":["Bob <bob@example.com>"],"bcc":["eve@example.com"],"subject":"Grüezi\r\nBcc: spy@evil.com","body":"Line 1\nLine 2"}`)
	if err != nil {
		t.Fatal(err)
	}
	msg := sentMessage(t, (*posted)[0])
	for _, want := range []string{
		"To: \"Bob\" <bob@example.com>\r\n", "Bcc: <eve@example.com>\r\n", "Subject: =?utf-8?q?Gr=C3=BCezi_Bcc:_spy@evil.com?=\r\n",
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n", "\r\n\r\nLine 1\r\nLine 2",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Count(msg, "Bcc:") != 2 {
		t.Errorf("the subject added a header:\n%s", msg)
	}
	if _, err := call(t, a, shared(f), box, "send_message", `{"to":["not an address"],"subject":"x","body":"y"}`); err == nil {
		t.Error("a bad address passed")
	}

	// A reply to all goes to the sender, copies the others but the mailbox,
	// and stays in the thread.
	if _, err := call(t, a, shared(f), box, "reply", `{"id":"m1","body":"Paid.","reply_all":true}`); err != nil {
		t.Fatal(err)
	}
	reply := (*posted)[1]
	msg = sentMessage(t, reply)
	if reply["threadId"] != "t1" || !strings.Contains(msg, "To: \"Bob\" <bob@example.com>\r\n") ||
		!strings.Contains(msg, "Cc: \"Cy\" <cy@example.com>, <dee@example.com>\r\n") || strings.Contains(msg, "ada@acme.com") ||
		!strings.Contains(msg, "Subject: Re: Invoice\r\n") || !strings.Contains(msg, "In-Reply-To: <m1@example.com>\r\n") ||
		!strings.Contains(msg, "Paid.\r\n\r\nOn Tue, 3 Nov 2026") || !strings.Contains(msg, "> Please pay.") {
		t.Errorf("reply:\n%s", msg)
	}

	if _, err := call(t, a, shared(f), box, "create_draft", `{"reply_to":"m1","body":"Will pay soon."}`); err != nil {
		t.Fatal(err)
	}
	msg = sentMessage(t, (*posted)[2])
	if !strings.Contains(msg, "To: \"Bob\" <bob@example.com>\r\n") || strings.Contains(msg, "Cc:") {
		t.Errorf("draft:\n%s", msg)
	}

	out, err := call(t, a, shared(f), box, "change_labels", `{"id":"m1","add":["invoices"],"remove":["INBOX","UNREAD"]}`)
	if err != nil || !strings.Contains(out, "Label_7") {
		t.Fatalf("change_labels = %s, %v", out, err)
	}
	if mod := (*posted)[3]; strings.Join(toStrings(mod["addLabelIds"]), ",") != "Label_7" ||
		strings.Join(toStrings(mod["removeLabelIds"]), ",") != "INBOX,UNREAD" {
		t.Errorf("modify = %v", mod)
	}
	if _, err := call(t, a, shared(f), box, "change_labels", `{"id":"m1","add":["Nope"]}`); err == nil {
		t.Error("an unknown label passed")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// A person's calls carry their own token, to their own mailbox.
func TestPerUser(t *testing.T) {
	f, a, _ := setup(t)
	user := f.UserConn(connectors.Gmail, "ws-token")
	if out, err := call(t, a, user, "me", "read_message", `{"id":"m1"}`); err != nil || !strings.Contains(out, "Invoice") {
		t.Errorf("read_message = %s, %v", out, err)
	}
	user.Token = ""
	if _, err := call(t, a, user, "me", "read_message", `{"id":"m1"}`); err == nil {
		t.Error("a call for nobody passed")
	}
	if err := a.CheckClient(context.Background(), f.UserConn(connectors.Gmail, "")); err != nil {
		t.Errorf("Check of the OAuth client = %v", err)
	}
}
