package confluence

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/atlassian"
)

func (a *Adapter) tools() apitool.Set {
	return apitool.Set{
		{
			Tool: channels.Tool{
				Name: "search",
				Description: "Search pages and blog posts by text, or anything with CQL. " +
					"A connector limited to one space searches only that space.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"text":{"type":"string","description":"Words to look for in pages and blog posts"},
					"cql":{"type":"string","description":"A CQL query instead, such as type = page AND title ~ \"onboarding\""},
					"limit":{"type":"integer","description":"At most this many results; default 25, up to 100"}}}`),
			},
			Run: atlassian.Run(a.search),
		},
		{
			Tool: channels.Tool{
				Name:        "get_page",
				Description: "Read a page or blog post as plain text, with its title, version and address.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"id":{"type":"string","description":"The page's id, a number"}},"required":["id"]}`),
			},
			Run: atlassian.Run(a.getPage),
		},
		{
			Tool: channels.Tool{
				Name:        "list_spaces",
				Description: "List the spaces this connector may use.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"limit":{"type":"integer","description":"At most this many spaces; default 25, up to 100"}}}`),
			},
			Run: atlassian.Run(a.listSpaces),
		},
		{
			Tool: channels.Tool{
				Name:        "create_page",
				Description: "Create a page.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"space":{"type":"string","description":"The space's key; only needed when the connector may use every space"},
					"title":{"type":"string"},
					"body":{"type":"string","description":"The content in Confluence storage format, which is XHTML such as <h2>Goals</h2><p>Text</p><ul><li>Item</li></ul>. Plain text becomes paragraphs."},
					"parent_id":{"type":"string","description":"The id of the page to put it under; empty for the top of the space"}},"required":["title","body"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.createPage),
		},
		{
			Tool: channels.Tool{
				Name:        "update_page",
				Description: "Replace a page's body, its title, or both. Read it with get_page first, so nothing is lost.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"id":{"type":"string","description":"The page's id"},
					"title":{"type":"string","description":"A new title; leave out to keep it"},
					"body":{"type":"string","description":"The whole new content in Confluence storage format (XHTML); leave out to keep it"}},"required":["id"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.updatePage),
		},
		{
			Tool: channels.Tool{
				Name:        "comment",
				Description: "Comment on a page or blog post.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"page_id":{"type":"string","description":"The page's id"},
					"body":{"type":"string","description":"Plain text, or Confluence storage format (XHTML)"}},"required":["page_id","body"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.comment),
		},
	}
}

// Hit is one search result.
type Hit struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Space    string `json:"space,omitempty"`
	URL      string `json:"url"`
	Excerpt  string `json:"excerpt,omitempty"`
	Modified string `json:"modified,omitempty"`
}

type searchIn struct {
	Text  string `json:"text"`
	CQL   string `json:"cql"`
	Limit int    `json:"limit"`
}

// highlight is how the search marks the words found.
var highlight = strings.NewReplacer("@@@hl@@@", "", "@@@endhl@@@", "")

func (a *Adapter) search(ctx context.Context, s atlassian.Site, target string, in searchIn) (any, error) {
	query := strings.TrimSpace(in.CQL)
	if text := strings.TrimSpace(in.Text); text != "" {
		if query != "" {
			return nil, errors.New("give text or cql, not both")
		}
		// siteSearch ranks like Cloud's search box; Data Center has only text.
		field := "text"
		if s.Cloud() {
			field = "siteSearch"
		}
		query = "type in (page, blogpost) AND " + field + " ~ " + atlassian.Quote(text)
	}
	if query == "" {
		return nil, errors.New("give text or cql")
	}
	clause := ""
	if target != channels.AnyTarget {
		clause = "space = " + atlassian.Quote(target)
	}
	cql, err := atlassian.Scope(clause, query, "")
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"cql":    {cql},
		"limit":  {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))},
		"expand": {"content.space"},
	}
	var res struct {
		Results []struct {
			Content *struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Space *space `json:"space"`
			} `json:"content"`
			Title        string `json:"title"`
			Excerpt      string `json:"excerpt"`
			URL          string `json:"url"`
			LastModified string `json:"lastModified"`
		} `json:"results"`
		TotalSize int `json:"totalSize"`
	}
	if err := a.call(ctx, s, http.MethodGet, "/search?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := []Hit{}
	for _, r := range res.Results {
		// Spaces and people come up in searches too; agents read content.
		if r.Content == nil || r.Content.ID == "" {
			continue
		}
		h := Hit{
			ID: r.Content.ID, Type: r.Content.Type, URL: base(s) + r.URL, Modified: r.LastModified,
			Title:   highlight.Replace(r.Title),
			Excerpt: atlassian.Cut(strings.Join(strings.Fields(html.UnescapeString(highlight.Replace(r.Excerpt))), " "), 500),
		}
		if r.Content.Space != nil {
			h.Space = r.Content.Space.Key
		}
		if target != channels.AnyTarget && h.Space != "" && !strings.EqualFold(h.Space, target) {
			continue
		}
		out = append(out, h)
	}
	return map[string]any{"results": out, "more": res.TotalSize > len(res.Results)}, nil
}

// Page is a page in full.
type Page struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Space     string `json:"space"`
	Version   int    `json:"version"`
	Updated   string `json:"updated,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	URL       string `json:"url"`
	Body      string `json:"body"`
}

type idIn struct {
	ID string `json:"id"`
}

func (a *Adapter) getPage(ctx context.Context, s atlassian.Site, target string, in idIn) (any, error) {
	c, err := a.page(ctx, s, target, in.ID, "version,body.storage")
	if err != nil {
		return nil, err
	}
	return Page{
		ID: c.ID, Type: c.Type, Title: c.Title, Space: c.Space.Key, Version: c.Version.Number,
		Updated: c.Version.When, UpdatedBy: c.Version.By.DisplayName, URL: base(s) + c.Links.WebUI,
		Body: atlassian.Cut(plainText(c.Body.Storage.Value), maxBody),
	}, nil
}

// Space is a space in a list.
type Space struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

func (sp space) view(s atlassian.Site) Space {
	return Space{Key: sp.Key, Name: sp.Name, Type: sp.Type, URL: base(s) + sp.Links.WebUI}
}

type listIn struct {
	Limit int `json:"limit"`
}

func (a *Adapter) listSpaces(ctx context.Context, s atlassian.Site, target string, in listIn) (any, error) {
	if target != channels.AnyTarget {
		sp, err := a.space(ctx, s, target)
		if err != nil {
			return nil, err
		}
		return []Space{sp.view(s)}, nil
	}
	var res struct {
		Results []space `json:"results"`
	}
	q := url.Values{"limit": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))}}
	if err := a.call(ctx, s, http.MethodGet, "/space?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := []Space{}
	for _, sp := range res.Results {
		out = append(out, sp.view(s))
	}
	return out, nil
}

// storage returns body in storage format. Plain text, which models often
// write for a comment, becomes paragraphs instead of failing to parse.
func storage(body string) map[string]any {
	if !strings.Contains(body, "<") {
		var ps []string
		for p := range strings.SplitSeq(strings.TrimSpace(body), "\n\n") {
			ps = append(ps, "<p>"+strings.ReplaceAll(html.EscapeString(strings.TrimSpace(p)), "\n", "<br/>")+"</p>")
		}
		body = strings.Join(ps, "")
	}
	return map[string]any{"storage": map[string]string{"value": body, "representation": "storage"}}
}

// written is what the write tools return.
type written struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Version int    `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
}

func (c content) written(s atlassian.Site) written {
	w := written{ID: c.ID, Title: c.Title, Version: c.Version.Number}
	if c.Links.WebUI != "" {
		w.URL = base(s) + c.Links.WebUI
	}
	return w
}

type createIn struct {
	Space    string `json:"space"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	ParentID string `json:"parent_id"`
}

func (a *Adapter) createPage(ctx context.Context, s atlassian.Site, target string, in createIn) (any, error) {
	key, err := spaceFor(target, in.Space)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("title", in.Title); err != nil {
		return nil, err
	}
	page := map[string]any{
		"type":  "page",
		"title": strings.TrimSpace(in.Title),
		"space": map[string]string{"key": key},
		"body":  storage(in.Body),
	}
	if strings.TrimSpace(in.ParentID) != "" {
		// Checked against the page's space, too, when the binding is to
		// every space, so the error says why.
		parent, err := a.page(ctx, s, key, in.ParentID, "")
		if err != nil {
			return nil, err
		}
		page["ancestors"] = []map[string]string{{"id": parent.ID}}
	}
	var c content
	if err := a.call(ctx, s, http.MethodPost, "/content", page, &c); err != nil {
		return nil, err
	}
	return c.written(s), nil
}

type updateIn struct {
	ID    string  `json:"id"`
	Title *string `json:"title"`
	Body  *string `json:"body"`
}

func (a *Adapter) updatePage(ctx context.Context, s atlassian.Site, target string, in updateIn) (any, error) {
	if in.Title == nil && in.Body == nil {
		return nil, errors.New("give a new title, body or both")
	}
	cur, err := a.page(ctx, s, target, in.ID, "version,body.storage")
	if err != nil {
		return nil, err
	}
	title, body := cur.Title, cur.Body.Storage.Value
	if in.Title != nil {
		if err := apitool.Need("title", *in.Title); err != nil {
			return nil, err
		}
		title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		body = *in.Body
	}
	var c content
	err = a.call(ctx, s, http.MethodPut, "/content/"+cur.ID, map[string]any{
		"id":      cur.ID,
		"type":    cur.Type,
		"title":   title,
		"space":   map[string]string{"key": cur.Space.Key},
		"version": map[string]int{"number": cur.Version.Number + 1},
		"body":    storage(body),
	}, &c)
	if err != nil {
		return nil, err
	}
	return c.written(s), nil
}

type commentIn struct {
	PageID string `json:"page_id"`
	Body   string `json:"body"`
}

func (a *Adapter) comment(ctx context.Context, s atlassian.Site, target string, in commentIn) (any, error) {
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	page, err := a.page(ctx, s, target, in.PageID, "")
	if err != nil {
		return nil, err
	}
	var c content
	err = a.call(ctx, s, http.MethodPost, "/content", map[string]any{
		"type":      "comment",
		"container": map[string]string{"id": page.ID, "type": page.Type},
		"body":      storage(in.Body),
	}, &c)
	if err != nil {
		return nil, err
	}
	return map[string]string{"id": c.ID, "page_id": page.ID}, nil
}
