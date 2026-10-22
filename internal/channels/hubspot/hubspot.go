// Package hubspot lets agents work in a HubSpot CRM: find, read, create and
// update contacts, companies, deals and tickets, and add notes to them. It
// calls the CRM API with a private app's access token, whose scopes are the
// limit of what agents may do.
//
// Endpoints:
//   - "api": the HubSpot API, default https://api.hubapi.com
package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// types are the objects agents work on, with the properties a record shows
// when the agent names none.
var types = map[string][]string{
	"contacts":  {"firstname", "lastname", "email", "phone", "company", "jobtitle", "lifecyclestage", "hubspot_owner_id"},
	"companies": {"name", "domain", "industry", "phone", "city", "country", "lifecyclestage", "hubspot_owner_id"},
	"deals":     {"dealname", "amount", "dealstage", "pipeline", "closedate", "hubspot_owner_id"},
	"tickets":   {"subject", "content", "hs_pipeline", "hs_pipeline_stage", "hs_ticket_priority", "hubspot_owner_id"},
}

// noteAssociation is HubSpot's own association type from a note to each
// object type. Naming them in the create call adds the note and its link in
// one step, so a failure cannot leave a note attached to nothing.
var noteAssociation = map[string]int{
	"contacts":  202,
	"companies": 190,
	"deals":     214,
	"tickets":   228,
}

// validID is a record's id. It goes into URLs.
var validID = regexp.MustCompile(`^[0-9]{1,20}$`)

// maxValue bounds one property's value in a result, so a long ticket or
// email body does not fill the model's context.
const maxValue = 2000

type Adapter struct {
	hc  *http.Client
	api string
	apitool.Set
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{hc: web.Client(opts), api: web.Endpoint(opts, "api", "https://api.hubapi.com")}
	a.Set = a.toolSet()
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.HubSpot,
		Label:    "HubSpot",
		Vendor:   "HubSpot",
		Category: "CRM",
		Summary:  "Find, read, create and update contacts, companies, deals and tickets, and add notes to them.",
		Fields: []channels.Field{{
			Name: "access_token", Label: "Access token", Secret: true, Required: true,
			Help: "The access token of a private app, starting with pat-.",
		}},
		Setup: "1. In HubSpot, open **Settings → Integrations → Private apps** (called legacy apps in newer accounts) and create an app for your agents.\n" +
			"2. Give it the scopes `crm.objects.contacts.read` and `.write`, the same for `companies` and `deals`, `tickets`, and `crm.objects.owners.read`. " +
			"Leave out the write scopes if agents should only read.\n" +
			"3. Copy the app's access token here.\n" +
			"4. Agents reach every record those scopes allow, so grant only what they need.",
	}
}

func token(conn channels.Connection) (string, error) {
	t := web.Value(conn, "access_token")
	if t == "" {
		return "", errors.New("the access token is missing")
	}
	return t, nil
}

func (a *Adapter) call(ctx context.Context, conn channels.Connection, method, path string, in, out any) error {
	tok, err := token(conn)
	if err != nil {
		return err
	}
	req, err := web.NewJSONRequest(ctx, method, a.api+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	return web.DoJSON(a.hc, req, "HubSpot", 16<<20, func(status int, b []byte) error {
		var e struct {
			Message  string `json:"message"`
			Category string `json:"category"`
		}
		_ = json.Unmarshal(b, &e)
		return &web.APIError{Service: "HubSpot", Status: status, Code: e.Category, Message: e.Message}
	}, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	err := a.call(ctx, conn, http.MethodGet, "/crm/v3/objects/contacts?limit=1", nil, nil)
	switch web.Status(err) {
	case http.StatusUnauthorized:
		return fmt.Errorf("HubSpot does not accept the access token; copy it again from the private app (%w)", err)
	case http.StatusForbidden:
		return fmt.Errorf("the private app may not read contacts; give it the scope crm.objects.contacts.read and the others in the steps (%w)", err)
	}
	return err
}

// Target is always everything: a kind with no TargetLabel has nothing to
// narrow to, and the API binds it to AnyTarget.
func (a *Adapter) Target(context.Context, channels.Connection, string) (string, error) {
	return channels.AnyTarget, nil
}

const objectType = `"object_type": {"type": "string", "enum": ["contacts", "companies", "deals", "tickets"]}`

const propertiesArg = `"properties": {"type": "array", "items": {"type": "string"},
	"description": "Property names to return. Empty returns the usual ones for the type."}`

const valuesArg = `"properties": {"type": "object", "additionalProperties": {"type": "string"},
	"description": "Property names and values, such as {\"firstname\": \"Ada\"}. Owners are set with hubspot_owner_id."}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "search",
			Description: "Search contacts, companies, deals or tickets by text and exact property values. Returns the matching records, newest first when there is no text.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectType + `,
				"query": {"type": "string", "description": "Words to look for, such as a name, email address or domain."},
				"filters": {"type": "object", "additionalProperties": {"type": "string"},
					"description": "Exact values the records must have, such as {\"dealstage\": \"closedwon\"}."},
				` + propertiesArg + `,
				"limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "How many records, 25 by default."},
				"after": {"type": "string", "description": "The next value of an earlier search, for the following page."}
			}, "required": ["object_type"]}`),
			Run: apitool.Typed(a.search),
		},
		{
			Name:        "get",
			Description: "Read one record by its id, with the ids of the contacts, companies, deals and tickets it is associated with.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectType + `,
				"id": {"type": "string"},
				` + propertiesArg + `
			}, "required": ["object_type", "id"]}`),
			Run: apitool.Typed(a.get),
		},
		{
			Name:        "create",
			Description: "Create a contact, company, deal or ticket.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectType + `,
				` + valuesArg + `
			}, "required": ["object_type", "properties"]}`),
			Write: true,
			Run:   apitool.Typed(a.create),
		},
		{
			Name:        "update",
			Description: "Change properties of a contact, company, deal or ticket. Properties left out keep their value.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectType + `,
				"id": {"type": "string"},
				` + valuesArg + `
			}, "required": ["object_type", "id", "properties"]}`),
			Write: true,
			Run:   apitool.Typed(a.update),
		},
		{
			Name:        "add_note",
			Description: "Add a note to a contact, company, deal or ticket, shown on its timeline.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + objectType + `,
				"id": {"type": "string", "description": "The record's id."},
				"body": {"type": "string", "description": "The note's text."}
			}, "required": ["object_type", "id", "body"]}`),
			Write: true,
			Run:   apitool.Typed(a.addNote),
		},
		{
			Name:        "list_owners",
			Description: "List the HubSpot users records can be assigned to, with the id to set as hubspot_owner_id.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"email": {"type": "string", "description": "Only the owner with this email address."},
				"limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "How many owners, 100 by default."},
				"after": {"type": "string", "description": "The next value of an earlier call, for the following page."}
			}}`),
			Run: apitool.Typed(a.listOwners),
		},
	}
}

// Record is a record as the model sees it: only properties with a value,
// each trimmed.
type Record struct {
	ID           string              `json:"id"`
	Properties   map[string]string   `json:"properties,omitempty"`
	Associations map[string][]string `json:"associations,omitempty"`
}

type record struct {
	ID           string             `json:"id"`
	Properties   map[string]*string `json:"properties"`
	Associations map[string]struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	} `json:"associations"`
}

func (r record) view() Record {
	out := Record{ID: r.ID, Properties: map[string]string{}}
	for k, v := range r.Properties {
		// hs_object_id repeats the id.
		if v == nil || *v == "" || k == "hs_object_id" {
			continue
		}
		out.Properties[k] = web.Cut(*v, maxValue, "…")
	}
	for kind, as := range r.Associations {
		seen := map[string]bool{}
		for _, x := range as.Results {
			if !seen[x.ID] {
				seen[x.ID] = true
				if out.Associations == nil {
					out.Associations = map[string][]string{}
				}
				out.Associations[kind] = append(out.Associations[kind], x.ID)
			}
		}
	}
	return out
}

// typeOf checks an object type, which goes into URLs.
func typeOf(t string) (string, error) {
	if _, ok := types[t]; !ok {
		return "", fmt.Errorf("%q is not an object type; use contacts, companies, deals or tickets", web.Short(t, 40))
	}
	return t, nil
}

func idOf(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%q is not a HubSpot record id; ids are numbers", web.Short(id, 40))
	}
	return id, nil
}

func propertiesOf(t string, asked []string) []string {
	var out []string
	for _, p := range asked {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return types[t]
	}
	return out
}

// SearchResult is one page of a search.
type SearchResult struct {
	Total   int      `json:"total"`
	Records []Record `json:"records"`
	// Next asks for the following page, as after.
	Next string `json:"next,omitempty"`
}

type page struct {
	Total   int      `json:"total"`
	Results []record `json:"results"`
	Paging  struct {
		Next struct {
			After string `json:"after"`
		} `json:"next"`
	} `json:"paging"`
}

func (a *Adapter) search(ctx context.Context, conn channels.Connection, _ string, in struct {
	ObjectType string            `json:"object_type"`
	Query      string            `json:"query"`
	Filters    map[string]string `json:"filters"`
	Properties []string          `json:"properties"`
	Limit      int               `json:"limit"`
	After      string            `json:"after"`
},
) (any, error) {
	t, err := typeOf(in.ObjectType)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"properties": propertiesOf(t, in.Properties),
		"limit":      apitool.Limit(in.Limit, 25, 100),
	}
	if q := strings.TrimSpace(in.Query); q != "" {
		body["query"] = q
	} else {
		// Without words to rank by, the newest records are the likeliest
		// wanted. Contacts name the property differently.
		modified := "hs_lastmodifieddate"
		if t == "contacts" {
			modified = "lastmodifieddate"
		}
		body["sorts"] = []map[string]string{{"propertyName": modified, "direction": "DESCENDING"}}
	}
	if len(in.Filters) > 0 {
		var fs []map[string]string
		for k, v := range in.Filters {
			fs = append(fs, map[string]string{"propertyName": k, "operator": "EQ", "value": v})
		}
		body["filterGroups"] = []map[string]any{{"filters": fs}}
	}
	if in.After != "" {
		body["after"] = in.After
	}
	var p page
	if err := a.call(ctx, conn, http.MethodPost, "/crm/v3/objects/"+t+"/search", body, &p); err != nil {
		return nil, err
	}
	out := SearchResult{Total: p.Total, Records: []Record{}, Next: p.Paging.Next.After}
	for _, r := range p.Results {
		out.Records = append(out.Records, r.view())
	}
	return out, nil
}

func (a *Adapter) get(ctx context.Context, conn channels.Connection, _ string, in struct {
	ObjectType string   `json:"object_type"`
	ID         string   `json:"id"`
	Properties []string `json:"properties"`
},
) (any, error) {
	t, err := typeOf(in.ObjectType)
	if err != nil {
		return nil, err
	}
	id, err := idOf(in.ID)
	if err != nil {
		return nil, err
	}
	var others []string
	for _, o := range slices.Sorted(maps.Keys(types)) {
		if o != t {
			others = append(others, o)
		}
	}
	q := url.Values{
		"properties":   {strings.Join(propertiesOf(t, in.Properties), ",")},
		"associations": {strings.Join(others, ",")},
	}
	var r record
	err = a.call(ctx, conn, http.MethodGet, "/crm/v3/objects/"+t+"/"+id+"?"+q.Encode(), nil, &r)
	if web.Status(err) == http.StatusNotFound {
		return nil, fmt.Errorf("there is no %s record with id %s", t, id)
	}
	if err != nil {
		return nil, err
	}
	return r.view(), nil
}

type change struct {
	ObjectType string            `json:"object_type"`
	ID         string            `json:"id"`
	Properties map[string]string `json:"properties"`
}

func (a *Adapter) create(ctx context.Context, conn channels.Connection, _ string, in change) (any, error) {
	t, err := typeOf(in.ObjectType)
	if err != nil {
		return nil, err
	}
	if len(in.Properties) == 0 {
		return nil, errors.New("give the new record's properties")
	}
	var r record
	if err := a.call(ctx, conn, http.MethodPost, "/crm/v3/objects/"+t, map[string]any{"properties": in.Properties}, &r); err != nil {
		return nil, err
	}
	return r.view(), nil
}

func (a *Adapter) update(ctx context.Context, conn channels.Connection, _ string, in change) (any, error) {
	t, err := typeOf(in.ObjectType)
	if err != nil {
		return nil, err
	}
	id, err := idOf(in.ID)
	if err != nil {
		return nil, err
	}
	if len(in.Properties) == 0 {
		return nil, errors.New("give the properties to change")
	}
	var r record
	err = a.call(ctx, conn, http.MethodPatch, "/crm/v3/objects/"+t+"/"+id, map[string]any{"properties": in.Properties}, &r)
	if web.Status(err) == http.StatusNotFound {
		return nil, fmt.Errorf("there is no %s record with id %s", t, id)
	}
	if err != nil {
		return nil, err
	}
	return r.view(), nil
}

func (a *Adapter) addNote(ctx context.Context, conn channels.Connection, _ string, in struct {
	ObjectType string `json:"object_type"`
	ID         string `json:"id"`
	Body       string `json:"body"`
},
) (any, error) {
	t, err := typeOf(in.ObjectType)
	if err != nil {
		return nil, err
	}
	id, err := idOf(in.ID)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	// HubSpot shows a note's body as HTML; the model writes plain text.
	body := strings.ReplaceAll(html.EscapeString(strings.TrimSpace(in.Body)), "\n", "<br>")
	note := map[string]any{
		"properties": map[string]string{
			"hs_note_body": body,
			"hs_timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"associations": []map[string]any{{
			"to": map[string]string{"id": id},
			"types": []map[string]any{{
				"associationCategory": "HUBSPOT_DEFINED", "associationTypeId": noteAssociation[t],
			}},
		}},
	}
	var r record
	if err := a.call(ctx, conn, http.MethodPost, "/crm/v3/objects/notes", note, &r); err != nil {
		return nil, err
	}
	return map[string]string{"note_id": r.ID}, nil
}

// Owner is someone records can be assigned to.
type Owner struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func (a *Adapter) listOwners(ctx context.Context, conn channels.Connection, _ string, in struct {
	Email string `json:"email"`
	Limit int    `json:"limit"`
	After string `json:"after"`
},
) (any, error) {
	q := url.Values{"limit": {fmt.Sprint(apitool.Limit(in.Limit, 100, 100))}}
	if e := strings.TrimSpace(in.Email); e != "" {
		q.Set("email", e)
	}
	if in.After != "" {
		q.Set("after", in.After)
	}
	var p struct {
		Results []struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"results"`
		Paging struct {
			Next struct {
				After string `json:"after"`
			} `json:"next"`
		} `json:"paging"`
	}
	if err := a.call(ctx, conn, http.MethodGet, "/crm/v3/owners?"+q.Encode(), nil, &p); err != nil {
		return nil, err
	}
	out := struct {
		Owners []Owner `json:"owners"`
		Next   string  `json:"next,omitempty"`
	}{Owners: []Owner{}, Next: p.Paging.Next.After}
	for _, o := range p.Results {
		out.Owners = append(out.Owners, Owner{
			ID: o.ID, Email: o.Email, Name: strings.TrimSpace(o.FirstName + " " + o.LastName),
		})
	}
	return out, nil
}
