// Package salesforce lets agents work in a Salesforce org: query it with
// SOQL, search it, read, describe, create and update records. It signs in
// with an External Client App's or Connected App's client credentials, as
// the integration user the app runs as, or as each person who signs in with
// their own account. That user's permissions are the limit of what agents
// may do.
//
// The org's My Domain URL is a connection setting, so there are no
// endpoints to override: tests point the setting at a fake server.
package salesforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// apiVersion is the REST API's version. Salesforce keeps old versions for
// years, so this changes only to use something new.
const apiVersion = "v62.0"

var (
	// validName is an object's or field's API name, such as Account or
	// Invoice__c. It goes into URLs.
	validName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)
	// validID is a record id, 15 or 18 characters.
	validID = regexp.MustCompile(`^[A-Za-z0-9]{15}([A-Za-z0-9]{3})?$`)
)

// maxValue bounds one field's value in a result, so a long description
// does not fill the model's context.
const maxValue = 2000

// lifetime is how long a token is kept. Salesforce does not say when it
// expires, and an org's sessions may last as little as 15 minutes; a token
// that stops working early is replaced on the first refusal.
const lifetime = 15 * time.Minute

type Adapter struct {
	hc     *http.Client
	tokens oauth.Cache
	apitool.Set
	oauth.Accounts
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{hc: web.SameServerClient(opts)}
	a.Set = a.toolSet()
	a.Accounts = oauth.NewAccounts(oauth.AccountsConfig{
		Service: "Salesforce",
		HTTP:    a.hc,
		SignIn: channels.SignIn{
			Fields: fields(),
			Setup: "1. In Salesforce Setup, create an **External Client App** with OAuth enabled, the callback URL {callback}, " +
				"and the scopes **Manage user data via APIs (api)**, **Perform requests at any time (refresh_token, offline_access)**, " +
				"**Access unique user identifiers (openid)** and **Access your email address (email)**.\n" +
				"2. In its policies, let the people who use agents sign in, and keep refresh tokens valid until revoked.\n" +
				"3. Copy the app's consumer key and secret, and your org's My Domain URL, here. " +
				"Then each person connects their own Salesforce account under Connectors.\n" +
				"4. Agents can do what that person may, through their profile, permission sets and sharing.",
		},
		App: func(conn channels.Connection) (oauth.App, error) {
			o, err := orgOf(conn)
			if err != nil {
				return oauth.App{}, err
			}
			return oauth.App{
				AuthURL: o.domain + "/services/oauth2/authorize", TokenURL: o.domain + "/services/oauth2/token",
				ClientID: o.clientID, Secret: o.secret,
				Scopes: []string{"api", "refresh_token", "openid", "email"},
			}, nil
		},
		// Salesforce does not say when a token expires.
		Lifetime: lifetime,
	})
	return a
}

func fields() []channels.Field {
	return []channels.Field{
		{
			Name: "domain_url", Label: "My Domain URL", Required: true, Endpoint: true,
			Help: "Your org's address, such as https://acme.my.salesforce.com.",
		},
		{
			Name: "client_id", Label: "Consumer key", Required: true,
			Help: "The app's consumer key, or client ID.",
		},
		{
			Name: "client_secret", Label: "Consumer secret", Secret: true, Required: true,
			Help: "The app's consumer secret, or client secret.",
		},
	}
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.Salesforce,
		Label:    "Salesforce",
		Vendor:   "Salesforce",
		Category: "CRM",
		Summary:  "Query, search, read, create and update records such as accounts, contacts, opportunities, leads and cases.",
		Fields:   fields(),
		Setup: "1. In Salesforce Setup, create a user for your agents, best with the **Salesforce Integration** user licence.\n" +
			"2. Give that user a permission set with **API Enabled** and access to only the objects and fields agents may read or change.\n" +
			"3. Create an **External Client App** (or a Connected App) with OAuth enabled and the scope **Manage user data via APIs (api)**.\n" +
			"4. In its policies, enable the **Client Credentials Flow** and set the user from step 1 as the run-as user.\n" +
			"5. Copy the app's consumer key and secret, and your org's My Domain URL, here.\n" +
			"6. Agents can do whatever that user may, so its permissions are the limit.",
	}
}

// org is a connection's settings.
type org struct {
	domain, clientID, secret string
}

func (o org) key() string { return web.Fingerprint(o.domain, o.clientID, o.secret) }

func orgOf(conn channels.Connection) (org, error) {
	o := org{clientID: web.Value(conn, "client_id"), secret: web.Value(conn, "client_secret")}
	raw := web.Value(conn, "domain_url")
	base, ok := web.BaseURL(raw)
	if raw == "" || !ok {
		return o, errors.New("the My Domain URL must be https://, such as https://acme.my.salesforce.com")
	}
	// People paste the address of any page in the org; only its server counts.
	u, _ := url.Parse(base)
	o.domain = "https://" + u.Host
	if o.clientID == "" || o.secret == "" {
		return o, errors.New("the consumer key and secret are missing")
	}
	return o, nil
}

// session is a token and the server to call with it. oauth.Cache keeps one
// string per key, so the server rides along with the token, after a space,
// which neither contains.
type session struct {
	token, instance string
}

func (a *Adapter) session(ctx context.Context, o org) (session, error) {
	v, err := a.tokens.Get(ctx, o.key(), func(ctx context.Context) (oauth.Token, error) {
		s, err := a.signIn(ctx, o)
		return oauth.Token{Access: s.instance + " " + s.token, Expiry: time.Now().Add(lifetime)}, err
	})
	if err != nil {
		return session{}, err
	}
	instance, token, _ := strings.Cut(v, " ")
	return session{token: token, instance: instance}, nil
}

// sessionFor is the session a call uses: the person's own, on a connector
// people sign in to, on the org's own server, or the run-as user's.
func (a *Adapter) sessionFor(ctx context.Context, conn channels.Connection, o org) (session, error) {
	if !conn.PerUser {
		return a.session(ctx, o)
	}
	if conn.Token == "" {
		return session{}, oauth.ErrNoToken
	}
	return session{token: conn.Token, instance: o.domain}, nil
}

// signIn asks for a token with the client credentials flow. It posts the
// form itself, since the answer's instance_url matters too.
func (a *Adapter) signIn(ctx context.Context, o org) (session, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {o.clientID},
		"client_secret": {o.secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.domain+"/services/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return session{}, fmt.Errorf("could not reach Salesforce to sign in: %w", err)
	}
	body, err := web.ReadBody(resp, 1<<20)
	if err != nil {
		return session{}, err
	}
	var r struct {
		AccessToken      string `json:"access_token"`
		InstanceURL      string `json:"instance_url"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != http.StatusOK || r.AccessToken == "" {
		return session{}, oauth.Describe("Salesforce", &oauth.Error{
			Status: resp.StatusCode, Code: r.Error, Description: web.Short(r.ErrorDescription, 300),
		})
	}
	instance, err := instanceOf(o.domain, r.InstanceURL)
	if err != nil {
		return session{}, err
	}
	return session{token: r.AccessToken, instance: instance}, nil
}

// instanceOf checks the server Salesforce says to call. Only its own
// servers are trusted, so a forged answer cannot send the token elsewhere.
func instanceOf(domain, instance string) (string, error) {
	if instance == "" {
		return domain, nil
	}
	u, err := url.Parse(instance)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("the sign-in answer named an instance URL that is not https://; it was not used")
	}
	d, _ := url.Parse(domain)
	host := u.Hostname()
	if u.Host != d.Host && !strings.HasSuffix(host, ".salesforce.com") && !strings.HasSuffix(host, ".force.com") {
		return "", fmt.Errorf("the sign-in answer named an instance on another server, %s; it was not used", web.Short(u.Host, 80))
	}
	return "https://" + u.Host, nil
}

func (a *Adapter) call(ctx context.Context, conn channels.Connection, method, path string, in, out any) error {
	o, err := orgOf(conn)
	if err != nil {
		return err
	}
	// A refused token has expired early; one more try with a new one is
	// safe, since a 401 means nothing was done.
	for try := 0; ; try++ {
		s, err := a.sessionFor(ctx, conn, o)
		if err != nil {
			return err
		}
		req, err := web.NewJSONRequest(ctx, method, s.instance+"/services/data/"+apiVersion+path, in)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Accept", "application/json")
		err = web.DoJSON(a.hc, req, "Salesforce", 16<<20, fail, out)
		if web.Status(err) != http.StatusUnauthorized || conn.PerUser {
			return err
		}
		a.tokens.Forget(o.key())
		if try == 1 {
			return err
		}
	}
}

// fail reads Salesforce's errors, a list of a code and a message.
func fail(status int, b []byte) error {
	var es []struct {
		Message   string `json:"message"`
		ErrorCode string `json:"errorCode"`
	}
	e := &web.APIError{Service: "Salesforce", Status: status}
	if json.Unmarshal(b, &es) == nil && len(es) > 0 {
		e.Code, e.Message = es[0].ErrorCode, es[0].Message
	}
	return e
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	o, err := orgOf(conn)
	if err != nil {
		return err
	}
	// Sign in afresh, so new credentials are what is checked.
	a.tokens.Forget(o.key())
	err = a.call(ctx, conn, http.MethodGet, "/", nil, nil)
	switch web.Status(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the API refused the run-as user; check it has the permission API Enabled (%w)", err)
	}
	return err
}

// Target is always everything: a kind with no TargetLabel has nothing to
// narrow to, and the API binds it to AnyTarget.
func (a *Adapter) Target(context.Context, channels.Connection, string) (string, error) {
	return channels.AnyTarget, nil
}

const objectArg = `"object": {"type": "string", "description": "The object's API name, such as Account, Opportunity or Invoice__c."}`

const idArg = `"id": {"type": "string", "description": "The record's 15 or 18 character id."}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{Tool: channels.Tool{
			Name:        "query",
			Description: "Run a SOQL query, such as SELECT Id, Name FROM Account WHERE Industry = 'Energy' LIMIT 20. Use describe to learn an object's fields.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"soql": {"type": "string"},
				"limit": {"type": "integer", "minimum": 1, "maximum": 200, "description": "How many records to return at most, 50 by default. Add LIMIT and OFFSET to the query for more."}
			}, "required": ["soql"]}`),
		}, Run: a.query},
		{Tool: channels.Tool{
			Name:        "search",
			Description: "Search accounts, contacts, opportunities, leads and cases for text, such as a name or email address. Or run your own SOSL.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"text": {"type": "string", "description": "What to look for."},
				"sosl": {"type": "string", "description": "A whole SOSL search instead of text, such as FIND {Acme} IN NAME FIELDS RETURNING Account(Id, Name)."},
				"limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "How many records at most, 25 by default."}
			}}`),
		}, Run: a.search},
		{Tool: channels.Tool{
			Name:        "get",
			Description: "Read one record by its id.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectArg + `, ` + idArg + `,
				"fields": {"type": "array", "items": {"type": "string"}, "description": "Fields to return. Empty returns every field with a value."}
			}, "required": ["object", "id"]}`),
		}, Run: a.get},
		{Tool: channels.Tool{
			Name:        "describe",
			Description: "List an object's fields: name, label, type, whether it is required or can be changed, and picklist values.",
			Parameters:  apitool.Schema(`{"type": "object", "properties": {` + objectArg + `}, "required": ["object"]}`),
		}, Run: a.describe},
		{Tool: channels.Tool{
			Name:        "create",
			Description: "Create a record.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectArg + `,
				"fields": {"type": "object", "description": "Field API names and values, such as {\"LastName\": \"Lovelace\", \"Company\": \"Acme\"}."}
			}, "required": ["object", "fields"]}`),
			Write: true,
		}, Run: a.create},
		{Tool: channels.Tool{
			Name:        "update",
			Description: "Change fields of a record. Fields left out keep their value.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectArg + `, ` + idArg + `,
				"fields": {"type": "object", "description": "Field API names and their new values."}
			}, "required": ["object", "id", "fields"]}`),
			Write: true,
		}, Run: a.update},
	}
}

func nameOf(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !validName.MatchString(s) {
		return "", fmt.Errorf("%q is not %s API name, such as Account or Invoice__c", web.Short(s, 80), what)
	}
	return s, nil
}

func recordOf(object, id string) (string, error) {
	o, err := nameOf("an object", object)
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%q is not a Salesforce record id; ids have 15 or 18 letters and digits", web.Short(id, 40))
	}
	return "/sobjects/" + o + "/" + id, nil
}

// clean readies a record for the model: without Salesforce's attributes,
// empty fields and long values.
func clean(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			if k == "attributes" || x == nil {
				continue
			}
			out[k] = clean(x)
		}
		return out
	case []any:
		for i, x := range v {
			v[i] = clean(x)
		}
		return v
	case string:
		return web.Cut(v, maxValue, "…")
	}
	return v
}

// QueryResult is a SOQL query's answer. Done is false when there are more
// records than returned.
type QueryResult struct {
	TotalSize int   `json:"total_size"`
	Done      bool  `json:"done"`
	Records   []any `json:"records"`
}

func (a *Adapter) query(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		SOQL  string `json:"soql"`
		Limit int    `json:"limit"`
	}](args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("soql", in.SOQL); err != nil {
		return nil, err
	}
	var r struct {
		TotalSize int               `json:"totalSize"`
		Done      bool              `json:"done"`
		Records   []json.RawMessage `json:"records"`
	}
	err = a.call(ctx, conn, http.MethodGet, "/query?"+url.Values{"q": {in.SOQL}}.Encode(), nil, &r)
	if errors.Is(err, web.ErrTooLarge) {
		return nil, errors.New("the answer is too large; select fewer fields or add a smaller LIMIT")
	}
	if err != nil {
		return nil, err
	}
	limit := apitool.Limit(in.Limit, 50, 200)
	out := QueryResult{TotalSize: r.TotalSize, Done: r.Done && len(r.Records) <= limit, Records: []any{}}
	for _, raw := range r.Records[:min(limit, len(r.Records))] {
		var rec any
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, err
		}
		out.Records = append(out.Records, clean(rec))
	}
	return out, nil
}

// soslReserved are the characters SOSL needs escaped in a search term.
var soslReserved = strings.NewReplacer(
	`\`, `\\`, `?`, `\?`, `&`, `\&`, `|`, `\|`, `!`, `\!`, `{`, `\{`, `}`, `\}`,
	`[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, `^`, `\^`, `~`, `\~`, `*`, `\*`,
	`:`, `\:`, `"`, `\"`, `'`, `\'`, `+`, `\+`, `-`, `\-`,
)

// returning is what a text search looks in, and the fields it shows.
const returning = "Account(Id, Name), Contact(Id, Name, Email), Opportunity(Id, Name, StageName), Lead(Id, Name, Company), Case(Id, CaseNumber, Subject)"

// Found is one record a search found.
type Found struct {
	Object string `json:"object"`
	Record any    `json:"record"`
}

func (a *Adapter) search(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Text  string `json:"text"`
		SOSL  string `json:"sosl"`
		Limit int    `json:"limit"`
	}](args)
	if err != nil {
		return nil, err
	}
	limit := apitool.Limit(in.Limit, 25, 100)
	q := strings.TrimSpace(in.SOSL)
	if q == "" {
		text := strings.TrimSpace(in.Text)
		if utf8.RuneCountInString(text) < 2 {
			return nil, errors.New("give text of at least two characters, or sosl")
		}
		q = fmt.Sprintf("FIND {%s} IN ALL FIELDS RETURNING %s LIMIT %d", soslReserved.Replace(text), returning, limit)
	}
	var r struct {
		SearchRecords []map[string]any `json:"searchRecords"`
	}
	if err := a.call(ctx, conn, http.MethodGet, "/search?"+url.Values{"q": {q}}.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []Found{}
	for _, rec := range r.SearchRecords[:min(limit, len(r.SearchRecords))] {
		f := Found{Record: clean(rec)}
		if at, ok := rec["attributes"].(map[string]any); ok {
			f.Object, _ = at["type"].(string)
		}
		out = append(out, f)
	}
	return out, nil
}

func (a *Adapter) get(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Object string   `json:"object"`
		ID     string   `json:"id"`
		Fields []string `json:"fields"`
	}](args)
	if err != nil {
		return nil, err
	}
	path, err := recordOf(in.Object, in.ID)
	if err != nil {
		return nil, err
	}
	var fields []string
	for _, f := range in.Fields {
		f, err := nameOf("a field", f)
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
	if len(fields) > 0 {
		path += "?" + url.Values{"fields": {strings.Join(fields, ",")}}.Encode()
	}
	var rec map[string]any
	if err := a.call(ctx, conn, http.MethodGet, path, nil, &rec); err != nil {
		return nil, err
	}
	return clean(rec), nil
}

// Object is what describe tells of an object.
type Object struct {
	Name   string  `json:"name"`
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
}

type Field struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Updateable  bool     `json:"updateable,omitempty"`
	ReferenceTo []string `json:"reference_to,omitempty"`
	Picklist    []string `json:"picklist,omitempty"`
}

// maxPicklist bounds a field's picklist values in describe, since some list
// every country or currency.
const maxPicklist = 50

func (a *Adapter) describe(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Object string `json:"object"`
	}](args)
	if err != nil {
		return nil, err
	}
	o, err := nameOf("an object", in.Object)
	if err != nil {
		return nil, err
	}
	var d struct {
		Name   string `json:"name"`
		Label  string `json:"label"`
		Fields []struct {
			Name              string   `json:"name"`
			Label             string   `json:"label"`
			Type              string   `json:"type"`
			Nillable          bool     `json:"nillable"`
			Createable        bool     `json:"createable"`
			Updateable        bool     `json:"updateable"`
			DefaultedOnCreate bool     `json:"defaultedOnCreate"`
			ReferenceTo       []string `json:"referenceTo"`
			PicklistValues    []struct {
				Value  string `json:"value"`
				Active bool   `json:"active"`
			} `json:"picklistValues"`
		} `json:"fields"`
	}
	if err := a.call(ctx, conn, http.MethodGet, "/sobjects/"+o+"/describe", nil, &d); err != nil {
		return nil, err
	}
	out := Object{Name: d.Name, Label: d.Label, Fields: []Field{}}
	for _, f := range d.Fields {
		field := Field{
			Name: f.Name, Label: f.Label, Type: f.Type, Updateable: f.Updateable, ReferenceTo: f.ReferenceTo,
			// Required is what a create must name: what Salesforce neither
			// leaves empty nor fills in.
			Required: f.Createable && !f.Nillable && !f.DefaultedOnCreate && f.Type != "boolean",
		}
		for _, p := range f.PicklistValues {
			if p.Active && len(field.Picklist) < maxPicklist {
				field.Picklist = append(field.Picklist, p.Value)
			}
		}
		out.Fields = append(out.Fields, field)
	}
	return out, nil
}

type change struct {
	Object string         `json:"object"`
	ID     string         `json:"id"`
	Fields map[string]any `json:"fields"`
}

func (a *Adapter) create(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[change](args)
	if err != nil {
		return nil, err
	}
	o, err := nameOf("an object", in.Object)
	if err != nil {
		return nil, err
	}
	if len(in.Fields) == 0 {
		return nil, errors.New("give the new record's fields")
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := a.call(ctx, conn, http.MethodPost, "/sobjects/"+o, in.Fields, &r); err != nil {
		return nil, err
	}
	return map[string]string{"id": r.ID}, nil
}

func (a *Adapter) update(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[change](args)
	if err != nil {
		return nil, err
	}
	path, err := recordOf(in.Object, in.ID)
	if err != nil {
		return nil, err
	}
	if len(in.Fields) == 0 {
		return nil, errors.New("give the fields to change")
	}
	if err := a.call(ctx, conn, http.MethodPatch, path, in.Fields, nil); err != nil {
		return nil, err
	}
	return map[string]any{"id": strings.TrimSpace(in.ID), "updated": true}, nil
}
