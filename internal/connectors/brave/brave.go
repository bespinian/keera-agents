// Package brave lets agents search the web and the news with the Brave
// Search API. A binding names nothing: the API searches the public web, and
// its key is all there is to protect.
//
// Endpoints:
//   - "api": the API's base URL, https://api.search.brave.com/res/v1
package brave

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

// The most a result's text may take, so a search does not fill the model's
// context.
const (
	maxTitle       = 300
	maxDescription = 1000
)

var (
	validCountry = regexp.MustCompile(`^[A-Z]{2}$`)
	// validLanguage is a code such as en, pt-br or zh-hans.
	validLanguage = regexp.MustCompile(`^[a-z]{2}(-[a-z]{2,4})?$`)
	validRange    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}to\d{4}-\d{2}-\d{2}$`)
	// tag is the markup Brave puts in snippets, such as <strong>.
	tag = regexp.MustCompile(`<[^>]*>`)
	// freshness is Brave's code for each period.
	freshness = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}
)

var _ connectors.API = (*Adapter)(nil)

type Adapter struct {
	hc  *http.Client
	api string
	apitool.Set
}

func New(opts connectors.Options) *Adapter {
	// Go keeps custom headers, such as the key's, on a redirect to another
	// server, so redirects stay on Brave's.
	a := &Adapter{
		hc:  web.SameServerClient(opts),
		api: web.Endpoint(opts, "api", "https://api.search.brave.com/res/v1"),
	}
	a.Set = a.toolSet()
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Brave,
		Label:    "Brave Search",
		Vendor:   "Brave",
		Category: "Web",
		Summary:  "Agents search the web and the news.",
		Fields: []connectors.Field{
			{
				Name: "api_key", Label: "API key", Secret: true, Required: true,
				Help: "A Brave Search API key.",
			},
		},
		Setup: "1. Sign up at api-dashboard.search.brave.com and choose a **Search** plan.\n" +
			"2. Under **API keys**, add a key and copy it here.\n" +
			"3. Agents read the pages they find with their **Web** tool, so give them that too.",
	}
}

// Check runs a search of one result, as Brave has no call that only checks a
// key. It counts as one query on the plan.
func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	key, err := apiKey(conn)
	if err != nil {
		return err
	}
	err = a.get(ctx, key, "/web/search", url.Values{"q": {"keera"}, "count": {"1"}}, nil)
	if badKey(err) {
		return fmt.Errorf("the API key does not work; copy it again from Brave's dashboard (%w)", err)
	}
	return err
}

func apiKey(conn connectors.Connection) (string, error) {
	if key := web.Value(conn, "api_key"); key != "" {
		return key, nil
	}
	return "", errors.New("the API key is missing")
}

// badKey reports whether Brave refused the key. It answers 422 with a code
// for a key it does not know.
func badKey(err error) bool {
	e, ok := errors.AsType[*web.APIError](err)
	return ok && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden ||
		strings.Contains(e.Code, "TOKEN"))
}

// Target has nothing to narrow: every binding searches the whole web.
func (a *Adapter) Target(context.Context, connectors.Connection, string) (string, error) {
	return connectors.AnyTarget, nil
}

func (a *Adapter) get(ctx context.Context, key, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.api+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	return web.DoJSON(a.hc, req, "Brave Search", 4<<20, func(status int, b []byte) error {
		var e struct {
			Error struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Error.Detail
		if msg == "" {
			msg = e.Message
		}
		return &web.APIError{Service: "Brave Search", Status: status, Code: e.Error.Code, Message: msg}
	}, out)
}

const searchArgs = `{"type":"object","properties":{
	"query":{"type":"string","description":"What to search for. Operators such as \"exact words\", -word and site:example.com work"},
	"count":{"type":"integer","description":"How many results; default 10, up to 20"},
	"page":{"type":"integer","description":"The page of results; default 1, up to 10"},
	"country":{"type":"string","description":"Results for one country, as two letters, such as CH"},
	"language":{"type":"string","description":"Only pages in one language, such as de"},
	"freshness":{"type":"string","description":"Only pages from the past day, week, month or year, or from a date range such as 2026-01-01to2026-03-31"}},
	"required":["query"]}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "search",
			Description: "Search the web. Returns each page's title, url and a snippet; fetch a page to read it.",
			Parameters:  apitool.Schema(searchArgs),
			Run:         a.find("/web/search"),
		},
		{
			Name:        "news",
			Description: "Search news articles. Returns each article's title, url, age and a snippet.",
			Parameters:  apitool.Schema(searchArgs),
			Run:         a.find("/news/search"),
		},
	}
}

type searchIn struct {
	Query     string `json:"query"`
	Count     int    `json:"count"`
	Page      int    `json:"page"`
	Country   string `json:"country"`
	Language  string `json:"language"`
	Freshness string `json:"freshness"`
}

// values checks the arguments and turns them into Brave's.
func (in searchIn) values() (url.Values, error) {
	if err := apitool.Need("query", in.Query); err != nil {
		return nil, err
	}
	q := url.Values{
		"q":     {strings.TrimSpace(in.Query)},
		"count": {strconv.Itoa(apitool.Limit(in.Count, 10, 20))},
		// Brave's offset counts pages, from 0.
		"offset": {strconv.Itoa(apitool.Limit(in.Page, 1, 10) - 1)},
	}
	if c := strings.ToUpper(strings.TrimSpace(in.Country)); c != "" {
		if !validCountry.MatchString(c) {
			return nil, fmt.Errorf("%q is not a country; give two letters, such as CH", web.Short(in.Country, 40))
		}
		q.Set("country", c)
	}
	if l := strings.ToLower(strings.TrimSpace(in.Language)); l != "" {
		if !validLanguage.MatchString(l) {
			return nil, fmt.Errorf("%q is not a language; give a code such as de", web.Short(in.Language, 40))
		}
		q.Set("search_lang", l)
	}
	if f := strings.ToLower(strings.TrimSpace(in.Freshness)); f != "" {
		code, ok := freshness[f]
		if !ok && !validRange.MatchString(f) {
			return nil, fmt.Errorf("%q is not a freshness; use day, week, month, year or a range such as 2026-01-01to2026-03-31",
				web.Short(in.Freshness, 40))
		}
		q.Set("freshness", cmp.Or(code, f))
	}
	return q, nil
}

// Result is one page or article found.
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	// Age is how old it is, as Brave puts it, such as "2 days ago".
	Age string `json:"age,omitempty"`
}

// Results are what a search found.
type Results struct {
	// SearchedFor is the query as Brave corrected it, when it did.
	SearchedFor string   `json:"searched_for,omitempty"`
	Results     []Result `json:"results"`
	More        bool     `json:"more,omitempty"`
}

// response is both endpoints' answer: web search puts its results under
// web, news search at the top.
type response struct {
	Query struct {
		Altered string `json:"altered"`
		More    bool   `json:"more_results_available"`
	} `json:"query"`
	Web struct {
		Results []Result `json:"results"`
	} `json:"web"`
	Results []Result `json:"results"`
}

// find is the tool that searches at path.
func (a *Adapter) find(path string) apitool.Run {
	return apitool.Typed(func(ctx context.Context, conn connectors.Connection, _ string, in searchIn) (any, error) {
		key, err := apiKey(conn)
		if err != nil {
			return nil, err
		}
		q, err := in.values()
		if err != nil {
			return nil, err
		}
		var res response
		if err := a.get(ctx, key, path, q, &res); err != nil {
			return nil, err
		}
		out := Results{SearchedFor: res.Query.Altered, Results: []Result{}, More: res.Query.More}
		for _, r := range append(res.Web.Results, res.Results...) {
			out.Results = append(out.Results, Result{
				Title: clean(r.Title, maxTitle), URL: r.URL, Description: clean(r.Description, maxDescription), Age: r.Age,
			})
		}
		return out, nil
	})
}

// clean turns a snippet's markup into plain text.
func clean(s string, n int) string {
	s = html.UnescapeString(tag.ReplaceAllString(s, ""))
	return web.Cut(strings.TrimSpace(s), n, "…")
}
