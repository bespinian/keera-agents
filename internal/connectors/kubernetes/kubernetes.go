// Package kubernetes lets agents look at and change one namespace of a
// Kubernetes cluster, with a service account's token, or as each person who
// signs in with the identity provider the cluster trusts. A person's calls
// carry their ID token, so the cluster's RBAC decides what they may.
//
// The API server's address is the connection's own setting, so the server
// passes the egress-guarded client, redirects are followed only on that
// server, and the address must be https://. A cluster with its own CA gets a
// client of its own that trusts that CA, never one that skips verification.
//
// Every call names the binding's namespace in its path. Cluster-scoped kinds,
// such as nodes or namespaces, are refused, so a binding never reaches past
// its namespace even with a token that could.
//
// The adapter has no endpoints to override: tests set the server field.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/oauth"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

// maxBody bounds an answer from the API server. Lists are cut to a limit,
// so only a huge object comes near it.
const maxBody = 16 << 20

var (
	// validNamespace is a DNS-1123 label, as Kubernetes names namespaces.
	validNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// validName is wider than DNS-1123, since some kinds allow colons or
	// capitals. It goes into paths, which escape it too.
	validName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._:-]*[A-Za-z0-9])?$`)
	// validAPIVersion is v1 or group/version, as discovery names them.
	validAPIVersion = regexp.MustCompile(`^(v1|[a-z0-9]([a-z0-9.-]*[a-z0-9])?/v[0-9]+((alpha|beta)[0-9]+)?)$`)
)

type Adapter struct {
	hc *http.Client

	// withCA are clients trusting a cluster's own CA, by its fingerprint.
	withCA *web.Cache[*http.Client]
	// discovery is what kinds each server serves, by server and apiVersion.
	discovery *web.Cache[[]resource]
	apitool.Set
	oauth.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{
		hc:        web.SameServerClient(opts),
		withCA:    web.NewCache[*http.Client](24*time.Hour, 100),
		discovery: web.NewCache[[]resource](10*time.Minute, 500),
	}
	a.Set = a.toolSet()
	a.Accounts = oauth.NewOIDC(a.hc).Accounts(
		"1. Configure the API server to trust your identity provider: --oidc-issuer-url, --oidc-client-id and "+
			"--oidc-username-claim=email, or a structured AuthenticationConfiguration. On EKS, associate an OIDC identity provider.\n"+
			"2. In the provider, register a web app for that client ID, with the redirect URI {callback}, and create a secret.\n"+
			"3. Bind each person, or their groups, to roles in the namespaces agents work in, as for their own kubectl.\n"+
			"4. Enter the API server, the issuer and the client here. Then each person connects their own account under Connectors.",
		serverField, caField)
	return a
}

// serverField and caField are settings of both kinds of connector.
var (
	serverField = connectors.Field{
		Name: "server", Label: "API server URL", Required: true, Endpoint: true,
		Help: "The cluster's API server, such as https://k8s.example.com:6443.",
	}
	caField = connectors.Field{
		Name: "ca_cert", Label: "CA certificate", Multiline: true,
		Help: "The cluster's CA certificate in PEM. Leave empty if the API server has a certificate from a public CA.",
	}
)

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Kubernetes,
		Label:    "Kubernetes",
		Vendor:   "Kubernetes",
		Category: "Cloud",
		Summary:  "Agents list, inspect and read the logs of workloads in a namespace, and can apply, scale and restart them.",
		Fields: []connectors.Field{
			serverField,
			{
				Name: "token", Label: "Service account token", Secret: true, Required: true, Multiline: true,
				Help: "The token of a service account, from a Secret of type kubernetes.io/service-account-token.",
			},
			{
				Name: "ca_cert", Label: "CA certificate", Multiline: true,
				Help: "The cluster's CA certificate in PEM, the ca.crt of the same Secret. Leave empty if the API server " +
					"has a certificate from a public CA.",
			},
		},
		Setup: "1. In each namespace agents work in, create a ServiceAccount, such as keera-agents.\n" +
			"2. Bind it to a role there with a RoleBinding: the ClusterRole view for read-only agents, " +
			"or edit for agents that apply, scale, restart and delete.\n" +
			"3. Create a long-lived token for it: a Secret of type kubernetes.io/service-account-token with the annotation " +
			"kubernetes.io/service-account.name set to the ServiceAccount's name.\n" +
			"4. Copy the Secret's token, and its ca.crt if the API server's certificate is not from a public CA.\n" +
			"5. If the API server is inside your network, ask your operator to name its host in " +
			"KEERA_AGENTS_UPSTREAM_PRIVATE.",
		TargetLabel: "Namespace",
		TargetHelp:  "The namespace agents work in, such as shop. Everything they do stays in it.",
	}
}

// cluster is a connection, ready to call.
type cluster struct {
	server, token string
	hc            *http.Client
}

func (a *Adapter) open(conn connectors.Connection) (cluster, error) {
	server, ok := web.BaseURL(web.Value(conn, "server"))
	if !ok {
		return cluster{}, errors.New("the API server URL must be https://, such as https://k8s.example.com:6443")
	}
	if server == "" {
		return cluster{}, errors.New("the API server URL is missing")
	}
	token, err := oauth.TokenOf(conn, "token", "the service account token is missing")
	if err != nil {
		return cluster{}, err
	}
	hc, err := a.client(web.Value(conn, "ca_cert"))
	return cluster{server: server, token: token, hc: hc}, err
}

// client returns a client that trusts ca, or the system's roots when ca is
// empty. It clones the server's transport, so the egress guard in its
// dialer still applies.
func (a *Adapter) client(ca string) (*http.Client, error) {
	if ca == "" {
		return a.hc, nil
	}
	key := web.Fingerprint(ca)
	if c, ok := a.withCA.Get(key, time.Now()); ok {
		return c, nil
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca)) {
		return nil, errors.New("the CA certificate is not a PEM certificate; paste the whole ca.crt, with its BEGIN and END lines")
	}
	base, ok := a.hc.Transport.(*http.Transport)
	if a.hc.Transport == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok {
		return nil, errors.New("this server cannot use a CA certificate of its own")
	}
	t := base.Clone()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	t.TLSClientConfig.RootCAs = roots
	c := *a.hc
	c.Transport = t
	a.withCA.Put(key, &c, time.Now())
	return &c, nil
}

// do sends a request and returns the body of a 2xx answer.
func (a *Adapter) do(ctx context.Context, c cluster, method, path, contentType string, body []byte, limit int64) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json, */*")
	return web.Do(c.hc, req, "Kubernetes", limit, apiError)
}

// call sends in as JSON with contentType, and decodes the answer into out
// unless it is nil.
func (a *Adapter) call(ctx context.Context, c cluster, method, path, contentType string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	b, err := a.do(ctx, c, method, path, contentType, body, maxBody)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// apiError reads Kubernetes' Status object. Its reason repeats the HTTP
// status, so only the message is kept.
func apiError(status int, body []byte) error {
	var s struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &s) != nil || s.Message == "" {
		s.Message = string(body)
	}
	return &web.APIError{Service: "Kubernetes", Status: status, Message: s.Message}
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	c, err := a.open(conn)
	if err != nil {
		return err
	}
	review := map[string]string{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}
	err = a.call(ctx, c, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", "application/json", review, nil)
	if web.Status(err) == http.StatusNotFound {
		// Before Kubernetes 1.28 there is no review; any authenticated call
		// proves the token.
		err = a.call(ctx, c, http.MethodGet, "/version", "", nil, nil)
	}
	switch web.Status(err) {
	case http.StatusUnauthorized:
		return fmt.Errorf("the cluster does not accept the token; check it is a service account's and has not expired (%w)", err)
	case http.StatusForbidden:
		return fmt.Errorf("the token is valid but may not ask who it is; check its roles (%w)", err)
	}
	return err
}

// namespace checks a binding's target, which is always one namespace.
func namespace(target string) (string, error) {
	ns := strings.ToLower(strings.TrimSpace(target))
	if len(ns) > 63 || !validNamespace.MatchString(ns) {
		return "", fmt.Errorf("%q is not a namespace; a namespace is lower-case letters, digits and dashes, such as shop", web.Short(target, 80))
	}
	return ns, nil
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	ns, err := namespace(target)
	if err != nil {
		return "", err
	}
	c, err := a.open(conn)
	if err != nil {
		return "", err
	}
	err = a.call(ctx, c, http.MethodGet, "/api/v1/namespaces/"+ns, "", nil, nil)
	if web.Status(err) == http.StatusForbidden {
		// A token bound only inside the namespace may not read the namespace
		// object itself. Listing its pods proves it can work there.
		err = a.call(ctx, c, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods?limit=1", "", nil, nil)
	}
	switch web.Status(err) {
	case http.StatusNotFound:
		return "", fmt.Errorf("the cluster has no namespace %s", ns)
	case http.StatusForbidden:
		if conn.PerUser {
			return "", fmt.Errorf("%s may not read namespace %s; bind them to a role there (%w)", conn.Who("the token"), ns, err)
		}
		return "", fmt.Errorf("the token may not read namespace %s; bind its service account to a role there (%w)", ns, err)
	case http.StatusUnauthorized:
		if conn.PerUser {
			return "", fmt.Errorf("the cluster does not accept the ID token of %s; check it trusts the identity provider "+
				"and the client ID (%w)", conn.Who("the token"), err)
		}
	}
	return ns, err
}

// resource is one kind a server serves, from discovery.
type resource struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Namespaced bool   `json:"namespaced"`
}

// kindRef is where a kind lives in the API.
type kindRef struct {
	apiVersion string
	// prefix is /api/v1 or /apis/{group}/{version}.
	prefix string
	resource
}

// defaultVersions saves the model naming the apiVersion of the kinds it
// asks for most. Other kinds need one.
var defaultVersions = map[string]string{
	"pod": "v1", "service": "v1", "configmap": "v1", "secret": "v1", "event": "v1",
	"persistentvolumeclaim": "v1", "serviceaccount": "v1", "endpoints": "v1",
	"deployment": "apps/v1", "statefulset": "apps/v1", "daemonset": "apps/v1", "replicaset": "apps/v1",
	"job": "batch/v1", "cronjob": "batch/v1", "ingress": "networking.k8s.io/v1",
	"networkpolicy": "networking.k8s.io/v1", "horizontalpodautoscaler": "autoscaling/v2",
	"poddisruptionbudget": "policy/v1", "role": "rbac.authorization.k8s.io/v1",
	"rolebinding": "rbac.authorization.k8s.io/v1",
}

// resolve finds a kind's resource through discovery, and refuses a
// cluster-scoped one, which no binding reaches.
func (a *Adapter) resolve(ctx context.Context, c cluster, apiVersion, kind, ns string) (kindRef, error) {
	kind = strings.TrimSpace(kind)
	if err := apitool.Need("kind", kind); err != nil {
		return kindRef{}, err
	}
	apiVersion = strings.TrimSpace(apiVersion)
	if apiVersion == "" {
		apiVersion = defaultVersions[strings.ToLower(kind)]
		if apiVersion == "" {
			return kindRef{}, fmt.Errorf("give the api_version of %s, such as apps/v1", web.Short(kind, 80))
		}
	}
	if !validAPIVersion.MatchString(apiVersion) {
		return kindRef{}, fmt.Errorf("%q is not an apiVersion; write v1 or group/version, such as apps/v1", web.Short(apiVersion, 80))
	}
	prefix := "/apis/" + apiVersion
	if apiVersion == "v1" {
		prefix = "/api/v1"
	}
	key := c.server + " " + apiVersion
	list, ok := a.discovery.Get(key, time.Now())
	if !ok {
		var d struct {
			Resources []resource `json:"resources"`
		}
		err := a.call(ctx, c, http.MethodGet, prefix, "", nil, &d)
		if web.Status(err) == http.StatusNotFound {
			return kindRef{}, fmt.Errorf("the cluster does not serve apiVersion %s", apiVersion)
		}
		if err != nil {
			return kindRef{}, err
		}
		for _, r := range d.Resources {
			// Subresources, such as pods/log, are not kinds of their own.
			if !strings.Contains(r.Name, "/") {
				list = append(list, r)
			}
		}
		a.discovery.Put(key, list, time.Now())
	}
	for _, r := range list {
		if strings.EqualFold(r.Kind, kind) {
			if !r.Namespaced {
				return kindRef{}, fmt.Errorf("%s is not in a namespace, and this connector works only within namespace %s", r.Kind, ns)
			}
			return kindRef{apiVersion: apiVersion, prefix: prefix, resource: r}, nil
		}
	}
	return kindRef{}, fmt.Errorf("apiVersion %s has no kind %s", apiVersion, web.Short(kind, 80))
}

// path is the path of the kind's objects in ns, or of one of them.
func (k kindRef) path(ns, name string) string {
	p := k.prefix + "/namespaces/" + ns + "/" + k.Name
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func checkName(what, name string) error {
	if err := apitool.Need(what, name); err != nil {
		return err
	}
	if len(name) > 253 || !validName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not a valid %s", web.Short(name, 80), what)
	}
	return nil
}
