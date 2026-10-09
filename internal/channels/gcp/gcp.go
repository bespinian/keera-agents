// Package gcp lets agents look into one Google Cloud project as a service
// account, or as each person who signs in with their own account: its
// resources, VMs, logs, metrics and Cloud Storage, and start and stop VMs.
//
// Every call names the binding's project in its path or scope. A bucket is
// used only after checking it belongs to that project, since bucket names
// are global and the connector may reach buckets elsewhere.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "resourcemanager": default https://cloudresourcemanager.googleapis.com
//   - "asset": Cloud Asset Inventory, default https://cloudasset.googleapis.com
//   - "compute": default https://compute.googleapis.com
//   - "logging": default https://logging.googleapis.com
//   - "monitoring": default https://monitoring.googleapis.com
//   - "storage": default https://storage.googleapis.com
package gcp

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

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/google"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	scope = "https://www.googleapis.com/auth/cloud-platform"
	// maxBody bounds an answer. Lists are cut to a limit, so only a huge
	// page comes near it.
	maxBody = 16 << 20
)

// validProject is a project ID, as Google allows them.
var validProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

type Adapter struct {
	auth  *google.Auth
	tools apitool.Set

	resourceManager, asset, compute, logging, monitoring, storage string

	// numbers are projects' numbers, by key and project ID, and buckets'
	// projects' numbers, by key and bucket. Neither changes.
	numbers *web.Cache[string]

	oauth.Accounts
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		auth:            google.NewAuth(opts),
		resourceManager: web.Endpoint(opts, "resourcemanager", "https://cloudresourcemanager.googleapis.com"),
		asset:           web.Endpoint(opts, "asset", "https://cloudasset.googleapis.com"),
		compute:         web.Endpoint(opts, "compute", "https://compute.googleapis.com"),
		logging:         web.Endpoint(opts, "logging", "https://logging.googleapis.com"),
		monitoring:      web.Endpoint(opts, "monitoring", "https://monitoring.googleapis.com"),
		storage:         web.Endpoint(opts, "storage", "https://storage.googleapis.com"),
		numbers:         web.NewCache[string](24*time.Hour, 1000),
	}
	a.tools = a.toolSet()
	a.Accounts = a.auth.Accounts(scope,
		"Cloud Resource Manager, Cloud Asset, Compute Engine, Cloud Logging and Cloud Monitoring APIs")
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GCP,
		Label:    "Google Cloud",
		Vendor:   "Google",
		Category: "Cloud",
		Summary:  "Agents search a project's resources, read its logs, metrics and Cloud Storage, and can start and stop its VMs.",
		Fields: []channels.Field{
			{
				Name: "service_account_json", Label: "Service account key", Secret: true, Required: true, Multiline: true,
				Help: "The JSON key file of a service account with access to the projects agents work in.",
			},
		},
		Setup: "1. In the project agents work in, create a service account, such as keera-agents.\n" +
			"2. Grant it the roles Viewer, Logs Viewer, Monitoring Viewer and Cloud Asset Viewer on the project. " +
			"Add Compute Instance Admin (v1) only if agents may start and stop VMs.\n" +
			"3. Create a JSON key for it and paste the whole file here.\n" +
			"4. Enable the Cloud Resource Manager, Cloud Asset, Compute Engine, Cloud Logging and Cloud Monitoring APIs " +
			"in the project the service account belongs to.\n" +
			"5. For more projects, grant the same roles there; one connector can serve them all.",
		TargetLabel: "Project",
		TargetHelp:  "The project ID, such as acme-prod-1234. Everything agents do stays in it.",
	}
}

func (a *Adapter) Tools() []channels.Tool { return a.tools.Tools() }

func (a *Adapter) Call(ctx context.Context, conn channels.Connection, target, tool string, args json.RawMessage) (any, error) {
	return a.tools.Call(ctx, conn, target, tool, args)
}

func key(conn channels.Connection) (string, error) {
	k := web.Value(conn, "service_account_json")
	if k == "" {
		return "", errors.New("the service account key is missing")
	}
	return k, nil
}

// cacheKey keeps what one connector learnt apart from another's.
func cacheKey(conn channels.Connection) string {
	if conn.PerUser {
		return "connection:" + conn.ID
	}
	k, _ := key(conn)
	return k
}

// who says whose access a call has, for errors.
func who(conn channels.Connection) string {
	switch {
	case !conn.PerUser:
		return "the service account"
	case conn.Account != "":
		return conn.Account
	}
	return "the person the agent works for"
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	k, err := key(conn)
	if err != nil {
		return err
	}
	return a.auth.Check(ctx, k, scope, "")
}

// send sends req as the service account, or the person the call is for,
// and returns the body of a 2xx answer.
func (a *Adapter) send(conn channels.Connection, req *http.Request, limit int64) ([]byte, error) {
	if conn.PerUser {
		if conn.Token == "" {
			return nil, oauth.ErrNoToken
		}
		return a.auth.DoAs(req, conn.Token, limit)
	}
	k, err := key(conn)
	if err != nil {
		return nil, err
	}
	return a.auth.Do(req, k, scope, "", limit)
}

// call sends in as JSON, unless it is nil, and decodes the answer into out.
func (a *Adapter) call(ctx context.Context, conn channels.Connection, method, u string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, u, in)
	if err != nil {
		return err
	}
	b, err := a.send(conn, req, maxBody)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// project checks a binding's target, which is always one project ID.
func project(target string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(target))
	if !validProject.MatchString(id) {
		return "", fmt.Errorf("%q is not a project ID; it is 6 to 30 lower-case letters, digits and dashes, such as acme-prod-1234", web.Short(target, 80))
	}
	return id, nil
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	id, err := project(target)
	if err != nil {
		return "", err
	}
	if _, err := a.lookup(ctx, conn, id); err != nil {
		return "", err
	}
	return id, nil
}

// lookup reads a project from Resource Manager and returns its number.
func (a *Adapter) lookup(ctx context.Context, conn channels.Connection, id string) (string, error) {
	var p struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	err := a.call(ctx, conn, http.MethodGet, a.resourceManager+"/v3/projects/"+id, nil, &p)
	// Google answers 403 for a project that does not exist, too, so as not
	// to say which IDs are taken.
	if s := web.Status(err); s == http.StatusForbidden || s == http.StatusNotFound {
		return "", fmt.Errorf("%s cannot see project %s; check the ID and grant the Viewer role there (%w)", who(conn), id, err)
	}
	if err != nil {
		return "", err
	}
	if p.State != "" && p.State != "ACTIVE" {
		return "", fmt.Errorf("project %s is %s, not active", id, strings.ToLower(strings.ReplaceAll(p.State, "_", " ")))
	}
	number, ok := strings.CutPrefix(p.Name, "projects/")
	if !ok || number == "" {
		return "", fmt.Errorf("the Resource Manager did not say project %s's number", id)
	}
	a.numbers.Put(web.Fingerprint(cacheKey(conn), "project", id), number, time.Now())
	return number, nil
}

// number is a project's number, cached.
func (a *Adapter) number(ctx context.Context, conn channels.Connection, id string) (string, error) {
	if n, ok := a.numbers.Get(web.Fingerprint(cacheKey(conn), "project", id), time.Now()); ok {
		return n, nil
	}
	return a.lookup(ctx, conn, id)
}

// validBucket is a bucket name, as Cloud Storage allows them. It goes into
// paths.
var validBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// bucketIn checks that bucket belongs to project id. Bucket names are
// global, so the connector may reach buckets of other projects.
func (a *Adapter) bucketIn(ctx context.Context, conn channels.Connection, id, bucket string) error {
	if err := apitool.Need("bucket", bucket); err != nil {
		return err
	}
	if !validBucket.MatchString(bucket) || strings.Contains(bucket, "..") {
		return fmt.Errorf("%q is not a bucket name", web.Short(bucket, 80))
	}
	want, err := a.number(ctx, conn, id)
	if err != nil {
		return err
	}
	bucketKey := web.Fingerprint(cacheKey(conn), "bucket", bucket)
	got, ok := a.numbers.Get(bucketKey, time.Now())
	if !ok {
		var b struct {
			ProjectNumber string `json:"projectNumber"`
		}
		err := a.call(ctx, conn, http.MethodGet, a.storage+"/storage/v1/b/"+url.PathEscape(bucket)+"?fields=projectNumber", nil, &b)
		if s := web.Status(err); s == http.StatusForbidden || s == http.StatusNotFound {
			return fmt.Errorf("%s cannot see bucket %s (%w)", who(conn), bucket, err)
		}
		if err != nil {
			return err
		}
		got = b.ProjectNumber
		a.numbers.Put(bucketKey, got, time.Now())
	}
	if got != want {
		return fmt.Errorf("bucket %s is not in project %s, and this connector works only within it", bucket, id)
	}
	return nil
}
