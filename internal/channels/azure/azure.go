// Package azure lets agents look into one Azure subscription through Azure
// Resource Manager: its resources, activity log, metrics and costs, and
// start and stop its virtual machines. It signs in as an Entra app with
// client credentials; what the app may do is set by its Azure roles.
//
// Each binding names one subscription, and every resource id an agent
// passes must be in it.
//
// Endpoints:
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "arm": Azure Resource Manager, default https://management.azure.com
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	scope   = "https://management.azure.com/.default"
	service = "Azure"

	subscriptionsVersion = "2022-12-01"
	resourcesVersion     = "2021-04-01"
)

var validSubscription = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Adapter struct {
	auth *microsoft.Auth
	arm  string
	set  apitool.Set
	// versions maps a resource type to the API version to read it with,
	// which only changes when Azure releases one.
	versions *web.Cache[string]
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		auth:     microsoft.NewAuth(opts),
		arm:      web.Endpoint(opts, "arm", "https://management.azure.com"),
		versions: web.NewCache[string](24*time.Hour, 1000),
	}
	a.set = a.tools()
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.Azure,
		Label:    "Azure",
		Vendor:   "Microsoft",
		Category: "Cloud",
		Summary:  "Agents look into an Azure subscription's resources, activity log, metrics and costs, and start and stop its virtual machines.",
		Fields: []channels.Field{
			{Name: "tenant_id", Label: "Tenant ID", Required: true, Help: "Your Microsoft Entra tenant ID."},
			{Name: "client_id", Label: "Client ID", Required: true, Help: "The application (client) ID of the app registration."},
			{
				Name: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "A client secret of the app registration, from Certificates & secrets.",
			},
		},
		Setup: "1. In Microsoft Entra, register an app and create a client secret.\n" +
			"2. In each subscription agents work in, open Access control (IAM) and assign the app the Reader role.\n" +
			"3. For costs, also assign it Cost Management Reader.\n" +
			"4. If agents may start and stop virtual machines, also assign it Virtual Machine Contributor, " +
			"on the subscription or only on the resource groups they may touch.\n" +
			"5. Bind each agent to a subscription by its ID.",
		TargetLabel: "Subscription",
		TargetHelp:  "The subscription's ID, such as 00000000-0000-0000-0000-000000000000.",
	}
}

func appOf(conn channels.Connection) (microsoft.App, error) {
	app := microsoft.App{
		Tenant:   web.Value(conn, "tenant_id"),
		ClientID: web.Value(conn, "client_id"),
		Secret:   web.Value(conn, "client_secret"),
		Scope:    scope,
	}
	if app.Tenant == "" || app.ClientID == "" || app.Secret == "" {
		return app, errors.New("the tenant ID, client ID or client secret is missing")
	}
	return app, nil
}

// subscription checks a binding's target and returns it in lower case.
func subscription(target string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(target))
	if s == channels.AnyTarget {
		return "", errors.New("an Azure connector must be bound to one subscription")
	}
	if !validSubscription.MatchString(s) {
		return "", fmt.Errorf("%q is not a subscription ID", web.Short(target, 80))
	}
	return s, nil
}

// query encodes ARM's options, keeping their $ and spaces as %20.
func query(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strings.ReplaceAll(url.QueryEscape(q[k]), "+", "%20")
	}
	return "?" + strings.Join(parts, "&")
}

// call sends one request to u, a full URL on Resource Manager, and decodes
// the answer into out unless it is nil.
func (a *Adapter) call(ctx context.Context, app microsoft.App, method, u string, in, out any) error {
	// Links ARM hands back must stay on ARM: the request carries the token.
	if !strings.HasPrefix(u, a.arm+"/") {
		return errors.New("the link does not point to Azure Resource Manager")
	}
	req, err := web.NewJSONRequest(ctx, method, u, in)
	if err != nil {
		return err
	}
	b, err := a.auth.Do(req, app, service, 16<<20)
	if err != nil || out == nil || len(b) == 0 {
		return err
	}
	return json.Unmarshal(b, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	app, err := appOf(conn)
	if err != nil {
		return err
	}
	// Sign in first and without the cache, so a wrong secret is reported as
	// such and not as an ARM error.
	if err := a.auth.Check(ctx, app); err != nil {
		return err
	}
	var r struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
		} `json:"value"`
	}
	if err := a.call(ctx, app, http.MethodGet, a.arm+"/subscriptions"+query(map[string]string{"api-version": subscriptionsVersion}), nil, &r); err != nil {
		return fmt.Errorf("signed in, but %w", err)
	}
	if len(r.Value) == 0 {
		return errors.New("signed in, but the app sees no subscription; assign it the Reader role on the subscriptions agents work in")
	}
	return nil
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	app, err := appOf(conn)
	if err != nil {
		return "", err
	}
	sub, err := subscription(target)
	if err != nil {
		return "", err
	}
	var r struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	err = a.call(ctx, app, http.MethodGet, a.arm+"/subscriptions/"+sub+query(map[string]string{"api-version": subscriptionsVersion}), nil, &r)
	switch st := web.Status(err); {
	case st == http.StatusNotFound || st == http.StatusForbidden || st == http.StatusUnauthorized:
		return "", fmt.Errorf("the app cannot see subscription %s; assign it the Reader role on it (%w)", sub, err)
	case err != nil:
		return "", err
	}
	return sub, nil
}

func (a *Adapter) Tools() []channels.Tool {
	return a.set.Tools()
}

func (a *Adapter) Call(ctx context.Context, conn channels.Connection, target, tool string, args json.RawMessage) (any, error) {
	return a.set.Call(ctx, conn, target, tool, args)
}

// run opens the connection and the binding's subscription for a tool.
func run(f func(ctx context.Context, app microsoft.App, sub string, args json.RawMessage) (any, error)) apitool.Run {
	return func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
		app, err := appOf(conn)
		if err != nil {
			return nil, err
		}
		sub, err := subscription(target)
		if err != nil {
			return nil, err
		}
		return f(ctx, app, sub, args)
	}
}

// resourceID is a resource id an agent passed, checked to be in the
// binding's subscription.
type resourceID struct {
	// id is as Azure writes it, with the subscription as bound.
	id string
	// path is id escaped for a URL.
	path string
	segs []string
	// namespace and typ are the resource's provider and type, such as
	// Microsoft.Compute and virtualMachines, or empty for a subscription
	// or a resource group.
	namespace, typ string
}

// parseID refuses an id outside the subscription sub, and anything that
// would change the URL beyond naming the resource.
func parseID(sub, id string) (resourceID, error) {
	id = strings.TrimSpace(id)
	segs := strings.Split(strings.Trim(id, "/"), "/")
	if !strings.HasPrefix(id, "/") || len(segs) < 2 || !strings.EqualFold(segs[0], "subscriptions") {
		return resourceID{}, fmt.Errorf("%q is not an Azure resource id, which starts /subscriptions/", web.Short(id, 120))
	}
	if !strings.EqualFold(segs[1], sub) {
		return resourceID{}, fmt.Errorf("%q is not in subscription %s, the only one this agent may use", web.Short(id, 120), sub)
	}
	segs[0], segs[1] = "subscriptions", sub
	esc := make([]string, len(segs))
	for i, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `?#%'\`) || strings.ContainsFunc(s, unicode.IsControl) {
			return resourceID{}, fmt.Errorf("%q is not a valid Azure resource id", web.Short(id, 120))
		}
		esc[i] = url.PathEscape(s)
	}
	r := resourceID{id: "/" + strings.Join(segs, "/"), path: "/" + strings.Join(esc, "/"), segs: segs}

	// Extension resources nest under another, so the last provider counts.
	p := -1
	for i, s := range segs {
		if strings.EqualFold(s, "providers") {
			p = i
		}
	}
	switch {
	case p < 0 && len(segs) == 2:
		return r, nil
	case p < 0 && len(segs) == 4 && strings.EqualFold(segs[2], "resourceGroups"):
		return r, nil
	case p < 0:
		return resourceID{}, fmt.Errorf("%q is not a resource id", web.Short(id, 120))
	}
	rest := segs[p+1:]
	// A namespace, then pairs of type and name.
	if len(rest) < 3 || len(rest)%2 == 0 {
		return resourceID{}, fmt.Errorf("%q names a list or a provider, not a resource", web.Short(id, 120))
	}
	r.namespace = rest[0]
	var types []string
	for i := 1; i < len(rest); i += 2 {
		types = append(types, rest[i])
	}
	r.typ = strings.Join(types, "/")
	return r, nil
}

// name is the resource's own name, the id's last part.
func (r resourceID) name() string {
	return r.segs[len(r.segs)-1]
}

// isVM is a virtual machine itself, not something under it.
func (r resourceID) isVM() bool {
	return strings.EqualFold(r.namespace, "Microsoft.Compute") && strings.EqualFold(r.typ, "virtualMachines")
}

// apiVersion finds the version to read a resource type with: the newest
// stable one its provider offers, or the newest preview if it has none.
func (a *Adapter) apiVersion(ctx context.Context, app microsoft.App, sub string, r resourceID) (string, error) {
	switch {
	case r.namespace == "" && len(r.segs) == 2:
		return subscriptionsVersion, nil
	case r.namespace == "":
		return resourcesVersion, nil
	}
	key := strings.ToLower(sub + "|" + r.namespace + "/" + r.typ)
	if v, ok := a.versions.Get(key, time.Now()); ok {
		return v, nil
	}
	var p struct {
		ResourceTypes []struct {
			ResourceType string   `json:"resourceType"`
			APIVersions  []string `json:"apiVersions"`
		} `json:"resourceTypes"`
	}
	u := a.arm + "/subscriptions/" + sub + "/providers/" + url.PathEscape(r.namespace) + query(map[string]string{"api-version": resourcesVersion})
	if err := a.call(ctx, app, http.MethodGet, u, nil, &p); err != nil {
		return "", fmt.Errorf("looking up the API version of %s/%s: %w", r.namespace, r.typ, err)
	}
	for _, t := range p.ResourceTypes {
		if !strings.EqualFold(t.ResourceType, r.typ) || len(t.APIVersions) == 0 {
			continue
		}
		vs := append([]string(nil), t.APIVersions...)
		// Versions are dates, so the newest sorts last.
		sort.Sort(sort.Reverse(sort.StringSlice(vs)))
		v := vs[0]
		for _, s := range vs {
			if !strings.Contains(strings.ToLower(s), "preview") {
				v = s
				break
			}
		}
		a.versions.Put(key, v, time.Now())
		return v, nil
	}
	return "", fmt.Errorf("there is no resource type %s/%s in Azure", r.namespace, r.typ)
}
