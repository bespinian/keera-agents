// Package aws lets agents work in one region of an AWS account: EC2
// instances, S3 buckets, CloudWatch alarms, metrics and logs, and what the
// region costs. It signs requests itself, with Signature Version 4, with an
// IAM user's access key, or with a role that key assumes.
//
// On a connector people sign in to, each person signs in with the identity
// provider the account trusts, and their ID token assumes the role in a
// session named after them. The role's trust policy decides who may, and
// CloudTrail shows who did what.
//
// Endpoints:
//   - "aws": every service, default https://{service}.{region}.amazonaws.com.
//     Requests are still signed for the real service and region, and S3 is
//     then reached by path, as {base}/{bucket}/{key}.
package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

var (
	validRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	validRole   = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]+$`)
)

const (
	// stsRegion is where keys are checked and roles assumed. Its sessions
	// work in every region, unlike those of the global endpoint.
	stsRegion  = "us-east-1"
	stsVersion = "2011-06-15"
	// sessionLife is how long an assumed role's session lasts, and
	// renewBefore how long before its end a new one is taken, so a call
	// never goes out with credentials about to expire.
	sessionLife = time.Hour
	renewBefore = 5 * time.Minute
	// maxResponse bounds what a call reads.
	maxResponse = 16 << 20
)

// labels name the services in errors.
var labels = map[string]string{
	"sts": "AWS STS", "ec2": "Amazon EC2", "s3": "Amazon S3", "monitoring": "Amazon CloudWatch",
	"logs": "CloudWatch Logs", "ce": "AWS Cost Explorer",
}

type Adapter struct {
	hc   *http.Client
	base string
	now  func() time.Time

	mu       sync.Mutex
	sessions map[string]session
	// buckets maps a bucket's name, unique across AWS, to its region.
	buckets *web.Cache[string]
	tools   apitool.Set
	oauth.Accounts
}

type session struct {
	creds creds
	exp   time.Time
}

func New(opts channels.Options) *Adapter {
	hc := *web.Client(opts)
	// AWS redirects only to another region or host, where the signature does
	// not hold. The answer itself says where to go, such as a bucket's region.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a := &Adapter{
		hc:       &hc,
		base:     web.Endpoint(opts, "aws", ""),
		now:      time.Now,
		sessions: map[string]session{},
		buckets:  web.NewCache[string](time.Hour, 10000),
	}
	a.tools = a.toolSet()
	a.Accounts = oauth.NewOIDC(a.hc).Accounts("the identity provider",
		"1. In IAM, add your identity provider as an **OpenID Connect** provider, with the client ID as its audience.\n"+
			"2. Create a role for web identity from that provider, with the permissions agents may use, as listed for a shared key. "+
			"Its trust policy decides who may assume it: narrow it with conditions on the token's sub or email.\n"+
			"3. In the provider, register a web app for that client ID, with the redirect URI {callback}, and create a secret.\n"+
			"4. Enter the issuer, the client and the role's ARN here. Then each person connects their own account under Connectors. "+
			"Each assumes the role in a session named after them, so CloudTrail shows who did what.",
	).WithFields(channels.Field{
		Name: "role_arn", Label: "Role ARN", Required: true,
		Help: "The role people assume with their sign-in, such as arn:aws:iam::123456789012:role/keera-people.",
	})
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.AWS,
		Label:    "AWS",
		Vendor:   "Amazon",
		Category: "Cloud",
		Summary: "Agents look at EC2 instances, S3 buckets, CloudWatch alarms, metrics and logs, and costs in one region, " +
			"and may start and stop instances and write to S3.",
		Fields: []channels.Field{
			{
				Name: "access_key_id", Label: "Access key ID", Required: true,
				Help: "Of an IAM user made for your agents, such as AKIAIOSFODNN7EXAMPLE.",
			},
			{Name: "secret_access_key", Label: "Secret access key", Secret: true, Required: true},
			{
				Name: "role_arn", Label: "Role ARN",
				Help: "Optional: a role the key assumes for every call, such as arn:aws:iam::123456789012:role/keera-agents. " +
					"Then the user needs only sts:AssumeRole.",
			},
		},
		Setup: "1. In IAM, create a user for your agents, without console access, and an access key for it.\n" +
			"2. Give it what agents read: the AWS managed policy **ReadOnlyAccess**, or a narrower one allowing " +
			"`ec2:DescribeRegions`, `ec2:DescribeInstances`, `s3:ListAllMyBuckets`, `s3:ListBucket`, `s3:GetObject`, " +
			"`cloudwatch:DescribeAlarms`, `cloudwatch:GetMetricStatistics` and `logs:FilterLogEvents`.\n" +
			"3. Only if agents may change things, also allow `ec2:StartInstances`, `ec2:StopInstances`, " +
			"`ec2:RebootInstances` and `s3:PutObject`, on just the instances and buckets they may change.\n" +
			"4. For costs, allow `ce:GetCostAndUsage`, and open Cost Explorer once in the Billing console to turn it on.\n" +
			"5. Optional: put these permissions on a role instead, trust the user in the role's trust policy, " +
			"allow the user only `sts:AssumeRole` on it, and enter the role's ARN.",
		TargetLabel: "Region",
		TargetHelp:  "The region agents work in, such as eu-central-1. Every call stays in it.",
	}
}

func (a *Adapter) Tools() []channels.Tool { return a.tools.Tools() }

func (a *Adapter) Call(ctx context.Context, conn channels.Connection, target, tool string, args json.RawMessage) (any, error) {
	return a.tools.Call(ctx, conn, target, tool, args)
}

// settingsOf reads the access key, and the role to assume, if any.
func settingsOf(conn channels.Connection) (creds, string, error) {
	c := creds{id: web.Value(conn, "access_key_id"), secret: web.Value(conn, "secret_access_key")}
	if c.id == "" || c.secret == "" {
		return c, "", errors.New("the access key ID and the secret access key are both needed")
	}
	role := web.Value(conn, "role_arn")
	if role != "" {
		if err := checkRole(role, "keera-agents"); err != nil {
			return c, "", err
		}
	}
	return c, role, nil
}

// checkRole says whether role is a role's ARN, with one named example in
// the error.
func checkRole(role, example string) error {
	if !validRole.MatchString(role) {
		return fmt.Errorf("%q is not a role's ARN; write one such as arn:aws:iam::123456789012:role/%s", web.Short(role, 100), example)
	}
	return nil
}

// credsOf returns what a connection's calls are signed with.
func (a *Adapter) credsOf(ctx context.Context, conn channels.Connection) (creds, error) {
	if conn.PerUser {
		return a.assumeAs(ctx, conn)
	}
	base, role, err := settingsOf(conn)
	if err != nil || role == "" {
		return base, err
	}
	return a.assume(ctx, base, role, "AssumeRole", url.Values{"RoleSessionName": {"keera-agents"}})
}

// assumeAs returns a session of the connection's role for the person a call
// is for, with their ID token, named after them.
func (a *Adapter) assumeAs(ctx context.Context, conn channels.Connection) (creds, error) {
	role := web.Value(conn, "role_arn")
	if conn.Token == "" {
		return creds{}, oauth.ErrNoToken
	}
	if err := checkRole(role, "keera-people"); err != nil {
		return creds{}, err
	}
	return a.assume(ctx, creds{}, role, "AssumeRoleWithWebIdentity", url.Values{
		"RoleSessionName":  {sessionName(conn.Account)},
		"WebIdentityToken": {conn.Token},
	})
}

// invalidSession are the characters a session's name may not hold.
var invalidSession = regexp.MustCompile(`[^\w+=,.@-]`)

// sessionName is a person's email as a role session's name, which CloudTrail
// shows: 2 to 64 of letters, digits and +=,.@_-.
func sessionName(account string) string {
	name := invalidSession.ReplaceAllString(account, "-")
	if len(name) < 2 {
		name = "keera-person"
	}
	return name[:min(len(name), 64)]
}

// assume returns a session of role, from the cache while it lasts. action
// is AssumeRole, signed with base, or AssumeRoleWithWebIdentity, which is
// not signed: the token in params proves who asks.
func (a *Adapter) assume(ctx context.Context, base creds, role, action string, params url.Values) (creds, error) {
	key := web.Fingerprint(base.id, base.secret, role, params.Get("WebIdentityToken"))
	now := a.now()
	a.mu.Lock()
	s, ok := a.sessions[key]
	a.mu.Unlock()
	if ok && now.Before(s.exp.Add(-renewBefore)) {
		return s.creds, nil
	}
	type credentials struct {
		ID         string    `xml:"AccessKeyId"`
		Secret     string    `xml:"SecretAccessKey"`
		Token      string    `xml:"SessionToken"`
		Expiration time.Time `xml:"Expiration"`
	}
	var out struct {
		Role credentials `xml:"AssumeRoleResult>Credentials"`
		Web  credentials `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	params.Set("Action", action)
	params.Set("RoleArn", role)
	params.Set("DurationSeconds", fmt.Sprint(int(sessionLife.Seconds())))
	c := client{a: a, creds: base, region: stsRegion}
	if err := c.query(ctx, "sts", stsVersion, params, &out); err != nil {
		return creds{}, err
	}
	got := out.Role
	if action != "AssumeRole" {
		got = out.Web
	}
	if got.ID == "" || got.Secret == "" {
		return creds{}, fmt.Errorf("AWS STS answered %s without credentials", action)
	}
	s = session{creds: creds{id: got.ID, secret: got.Secret, token: got.Token}, exp: got.Expiration}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, old := range a.sessions {
		if now.After(old.exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[key] = s
	return s.creds, nil
}

type identity struct {
	Account string `json:"account" xml:"GetCallerIdentityResult>Account"`
	ARN     string `json:"arn" xml:"GetCallerIdentityResult>Arn"`
}

func (c client) identity(ctx context.Context) (identity, error) {
	var out identity
	err := c.query(ctx, "sts", stsVersion, url.Values{"Action": {"GetCallerIdentity"}}, &out)
	return out, err
}

// CheckClient also checks the role's ARN. The role is first assumed when
// someone binds a region.
func (a *Adapter) CheckClient(ctx context.Context, conn channels.Connection) error {
	if err := checkRole(web.Value(conn, "role_arn"), "keera-people"); err != nil {
		return err
	}
	return a.Accounts.CheckClient(ctx, conn)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	base, role, err := settingsOf(conn)
	if err != nil {
		return err
	}
	me, err := client{a: a, creds: base, region: stsRegion}.identity(ctx)
	switch code(err) {
	case "":
	case "InvalidClientTokenId":
		return fmt.Errorf("AWS does not know the access key ID; check it, and that the key is active (%w)", err)
	case "SignatureDoesNotMatch":
		return fmt.Errorf("the secret access key does not belong to the access key ID (%w)", err)
	default:
		if web.Status(err) == http.StatusForbidden {
			return fmt.Errorf("AWS refused the access key (%w)", err)
		}
	}
	if err != nil || role == "" {
		return err
	}
	assumed, err := a.assume(ctx, base, role, "AssumeRole", url.Values{"RoleSessionName": {"keera-agents"}})
	if web.Status(err) == http.StatusForbidden {
		return fmt.Errorf("%s may not assume %s; allow it sts:AssumeRole on the role, and trust it in the role's trust policy (%w)",
			me.ARN, role, err)
	}
	if err != nil {
		return err
	}
	_, err = client{a: a, creds: assumed, region: stsRegion}.identity(ctx)
	return err
}

// regionOf checks a binding's target, a region.
func regionOf(target string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(target))
	if !validRegion.MatchString(r) {
		return "", fmt.Errorf("%q is not an AWS region; write one such as eu-central-1", web.Short(target, 40))
	}
	return r, nil
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	c, err := a.client(ctx, conn, target)
	if err != nil {
		return "", err
	}
	var out struct {
		Regions []string `xml:"regionInfo>item>regionName"`
	}
	err = c.query(ctx, "ec2", ec2Version, url.Values{"Action": {"DescribeRegions"}, "RegionName.1": {c.region}}, &out)
	if code(err) == "UnauthorizedOperation" {
		// Without EC2, STS answering in the region shows it is enabled too.
		if _, err := c.identity(ctx); err == nil {
			return c.region, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("could not use %s; check the region exists and is enabled for the account (%w)", c.region, err)
	}
	for _, r := range out.Regions {
		if r == c.region {
			return c.region, nil
		}
	}
	return "", fmt.Errorf("%s is not enabled for the account; enable it in the AWS console under Account, AWS Regions", c.region)
}

// client is the adapter with one call's credentials and region. Every
// request it makes goes to that region, except Cost Explorer's.
type client struct {
	a      *Adapter
	creds  creds
	region string
}

func (a *Adapter) client(ctx context.Context, conn channels.Connection, target string) (client, error) {
	region, err := regionOf(target)
	if err != nil {
		return client{}, err
	}
	cr, err := a.credsOf(ctx, conn)
	return client{a: a, creds: cr, region: region}, err
}

// endpoint is a service's base URL in the client's region.
func (c client) endpoint(service string) string {
	if c.a.base != "" {
		return c.a.base
	}
	return "https://" + service + "." + c.region + ".amazonaws.com"
}

// request makes a signed request. header must hold every header to send.
func (c client) request(ctx context.Context, service, method, u string, body []byte, header http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	// Only AssumeRoleWithWebIdentity goes without credentials.
	if c.creds.id != "" {
		sign(req, body, c.creds, c.region, service, c.a.now())
	}
	return req, nil
}

// send makes a signed request and returns the answer, whatever its status.
func (c client) send(ctx context.Context, service, method, u string, body []byte, header http.Header) (*http.Response, error) {
	req, err := c.request(ctx, service, method, u, body, header)
	if err != nil {
		return nil, err
	}
	resp, err := web.Call(c.a.hc, req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", labels[service], err)
	}
	return resp, nil
}

// do makes a signed request and returns the body of a 2xx answer.
func (c client) do(ctx context.Context, service, method, u string, body []byte, header http.Header) ([]byte, error) {
	req, err := c.request(ctx, service, method, u, body, header)
	if err != nil {
		return nil, err
	}
	return web.Do(c.a.hc, req, labels[service], maxResponse, func(status int, b []byte) error {
		return apiError(service, status, b)
	})
}

// query calls a service with the Query API, as EC2, STS and CloudWatch take
// it, and decodes the XML answer into out.
func (c client) query(ctx context.Context, service, version string, params url.Values, out any) error {
	params.Set("Version", version)
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	b, err := c.do(ctx, service, http.MethodPost, c.endpoint(service)+"/", []byte(encodeQuery(params)), h)
	if err != nil {
		return err
	}
	return xml.Unmarshal(b, out)
}

// callJSON calls a service with AWS's JSON protocol, as CloudWatch Logs and
// Cost Explorer take it. target names the operation.
func (c client) callJSON(ctx context.Context, service, target string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/x-amz-json-1.1")
	h.Set("X-Amz-Target", target)
	b, err := c.do(ctx, service, http.MethodPost, c.endpoint(service)+"/", body, h)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func apiError(service string, status int, body []byte) error {
	code, msg := errorDetail(body)
	return &web.APIError{Service: labels[service], Status: status, Code: code, Message: msg}
}

// errorDetail reads AWS's error code and message. The JSON protocol puts
// them in __type and message. The XML ones nest Code and Message, each
// service a little differently, so the first of each is taken.
func errorDetail(body []byte) (code, msg string) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		// It may be namespaced, as in com.amazonaws.logs#ResourceNotFoundException.
		return e.Type[strings.LastIndexByte(e.Type, '#')+1:], e.Message
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	var field string
	for {
		tok, err := d.Token()
		if err != nil {
			return code, msg
		}
		switch t := tok.(type) {
		case xml.StartElement:
			field = t.Name.Local
		case xml.EndElement:
			field = ""
		case xml.CharData:
			s := strings.TrimSpace(string(t))
			if field == "Code" && code == "" {
				code = s
			} else if field == "Message" && msg == "" {
				msg = s
			}
		}
	}
}

// code is the AWS error code of a refused call, or "".
func code(err error) string {
	if e, ok := errors.AsType[*web.APIError](err); ok {
		return e.Code
	}
	return ""
}
