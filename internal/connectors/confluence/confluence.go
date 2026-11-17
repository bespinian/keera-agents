// Package confluence lets agents search, read, write and comment on
// Confluence pages, with an API token on Confluence Cloud or a personal
// access token on Confluence Data Center, or, on Confluence Cloud, as each
// person who signs in with their own account.
//
// It calls the REST API version 1 content API, which both have: on Cloud
// at <site>/wiki/rest/api, on Data Center at <base>/rest/api. A binding
// names a space; every call stays in it.
//
// It calls the connection's site URL, or for a person, Atlassian's API
// gateway, whose endpoints package atlassian lists.
package confluence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/atlassian"
	"github.com/bespinian/keera-agents/internal/connectors/internal/oauth"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

var (
	// validID and validSpace go into URLs and queries. Personal spaces'
	// keys start with a tilde.
	validID    = regexp.MustCompile(`^[0-9]{1,20}$`)
	validSpace = regexp.MustCompile(`^~?[A-Za-z0-9_:-]{1,255}$`)
)

// maxBody bounds the text get_page returns, so one page does not fill the
// model's context.
const maxBody = 50000

var (
	_ connectors.API     = (*Adapter)(nil)
	_ connectors.PerUser = (*Adapter)(nil)
)

type Adapter struct {
	c *atlassian.Client
	apitool.Set
	oauth.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{c: atlassian.New(opts, "Confluence")}
	a.Set = a.toolSet()
	a.Accounts = a.c.Accounts("search:confluence", "read:confluence-content.all", "read:confluence-space.summary",
		"write:confluence-content", "read:confluence-user")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Confluence,
		Label:    "Confluence",
		Vendor:   "Atlassian",
		Category: "Projects and wikis",
		Summary:  "Agents search and read pages, write new ones, update them and comment on them.",
		Fields:   atlassian.Fields("Confluence"),
		Setup: "1. Create an Atlassian account for your agents and give it access to the spaces they work in: " +
			"View to read; to write, also Add pages and Add comments.\n" +
			"2. On Atlassian Cloud, sign in as that account and create an API token, without scopes, " +
			"at https://id.atlassian.com/manage-profile/security/api-tokens. Enter it with the account's email, " +
			"and the site's address without /wiki.\n" +
			"3. On Confluence Data Center, sign in as that account and create a personal access token in your profile. " +
			"Leave the email empty.",
		TargetLabel: "Space",
		TargetHelp:  "The space's key, such as DOCS.",
		AnyTarget:   true,
	}
}

// base is where the API and the pages are: Cloud serves Confluence under
// /wiki, which people may or may not paste with the site.
func base(s atlassian.Site) string {
	if s.Cloud() {
		return strings.TrimSuffix(s.Base, "/wiki") + "/wiki"
	}
	return s.Base
}

func (a *Adapter) call(ctx context.Context, s atlassian.Site, method, path string, in, out any) error {
	api := base(s)
	if s.API != "" {
		// The gateway serves Confluence under /wiki too.
		api = s.API + "/wiki"
	}
	return a.c.Call(ctx, s, method, api+"/rest/api"+path, in, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	s, err := a.c.Open(ctx, conn)
	if err != nil {
		return err
	}
	var me struct {
		Type string `json:"type"`
	}
	if err := a.call(ctx, s, http.MethodGet, "/user/current", nil, &me); err != nil {
		return a.c.CheckError(s, err)
	}
	// A site open to anonymous readers answers as nobody instead of 401.
	if me.Type == "anonymous" {
		return errors.New("the request reached Confluence as nobody; check the email and API token")
	}
	return nil
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	if target == connectors.AnyTarget {
		return target, nil
	}
	s, err := a.c.Open(ctx, conn)
	if err != nil {
		return "", err
	}
	sp, err := a.space(ctx, s, target)
	if err != nil {
		return "", err
	}
	return sp.Key, nil
}

type space struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Links links  `json:"_links"`
}

type links struct {
	WebUI string `json:"webui"`
}

func (a *Adapter) space(ctx context.Context, s atlassian.Site, key string) (space, error) {
	key = strings.TrimSpace(key)
	if !validSpace.MatchString(key) {
		return space{}, fmt.Errorf("%q is not a space key, such as DOCS", web.Short(key, 40))
	}
	var sp space
	err := a.call(ctx, s, http.MethodGet, "/space/"+key, nil, &sp)
	if web.Status(err) == http.StatusNotFound {
		return space{}, fmt.Errorf("the account cannot see a Confluence space %s; check the key and the account's access", key)
	}
	return sp, err
}

type content struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Space   space  `json:"space"`
	Version struct {
		Number int    `json:"number"`
		When   string `json:"when"`
		By     struct {
			DisplayName string `json:"displayName"`
		} `json:"by"`
	} `json:"version"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Links links `json:"_links"`
}

// page reads a page, with expand naming what besides its space, and
// refuses it outside the binding's space.
func (a *Adapter) page(ctx context.Context, s atlassian.Site, target, id, expand string) (content, error) {
	if expand != "" {
		expand = "," + expand
	}
	id = strings.TrimSpace(id)
	if !validID.MatchString(id) {
		return content{}, fmt.Errorf("%q is not a page id, a number such as 123456", web.Short(id, 40))
	}
	var c content
	if err := a.call(ctx, s, http.MethodGet, "/content/"+id+"?expand=space"+expand, nil, &c); err != nil {
		return content{}, err
	}
	if target != connectors.AnyTarget && !strings.EqualFold(c.Space.Key, target) {
		return content{}, fmt.Errorf("page %s is not in space %s, the only space this connector may use", id, target)
	}
	return c, nil
}

// spaceFor is the space a new page goes in: the binding's, or the one
// named when the binding is to every space.
func spaceFor(target, named string) (string, error) {
	named = strings.TrimSpace(named)
	if target != connectors.AnyTarget {
		if named != "" && !strings.EqualFold(named, target) {
			return "", fmt.Errorf("this connector may use only space %s", target)
		}
		return target, nil
	}
	if !validSpace.MatchString(named) {
		return "", errors.New("give space, the key of the space, such as DOCS")
	}
	return named, nil
}
