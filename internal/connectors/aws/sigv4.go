package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// creds sign requests: an access key, and a session token when they come
// from STS.
type creds struct {
	id, secret, token string
}

const (
	algorithm = "AWS4-HMAC-SHA256"
	amzDate   = "20060102T150405Z"
)

// sign adds AWS Signature Version 4 to req, whose body is body. It signs
// host and every header req has, so set them all first.
func sign(req *http.Request, body []byte, c creds, region, service string, now time.Time) {
	t := now.UTC()
	req.Header.Set("X-Amz-Date", t.Format(amzDate))
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}
	payload := hexSHA256(body)
	// S3 refuses a request without its payload's hash. The other services
	// do not need it, and AWS's test vectors leave it out.
	if service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payload)
	}
	names := []string{"host"}
	for k := range req.Header {
		names = append(names, strings.ToLower(k))
	}
	slices.Sort(names)
	names = slices.Compact(names)
	req.Header.Set("Authorization", algorithm+" Credential="+c.id+"/"+scope(t, region, service)+
		", SignedHeaders="+strings.Join(names, ";")+
		", Signature="+signature(req, names, payload, c.secret, region, service, t))
}

func scope(t time.Time, region, service string) string {
	return t.Format("20060102") + "/" + region + "/" + service + "/aws4_request"
}

// signature is the signature of req over the headers names, which are lower
// case and sorted.
func signature(req *http.Request, names []string, payload, secret, region, service string, t time.Time) string {
	toSign := algorithm + "\n" + t.Format(amzDate) + "\n" + scope(t, region, service) + "\n" +
		hexSHA256([]byte(canonicalRequest(req, names, payload, service == "s3")))
	key := hmacSHA256([]byte("AWS4"+secret), t.Format("20060102"))
	for _, part := range []string{region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func canonicalRequest(req *http.Request, names []string, payload string, s3 bool) string {
	var b strings.Builder
	b.WriteString(req.Method + "\n" + canonicalURI(req.URL, s3) + "\n")
	q, _ := url.ParseQuery(req.URL.RawQuery)
	b.WriteString(encodeQuery(q) + "\n")
	for _, n := range names {
		v := req.Host
		if v == "" {
			v = req.URL.Host
		}
		if n != "host" {
			vs := req.Header.Values(n)
			for i := range vs {
				vs[i] = strings.Join(strings.Fields(vs[i]), " ")
			}
			v = strings.Join(vs, ",")
		}
		b.WriteString(n + ":" + v + "\n")
	}
	b.WriteString("\n" + strings.Join(names, ";") + "\n" + payload)
	return b.String()
}

// canonicalURI encodes the path once for S3 and twice for the rest, as AWS
// asks. Other services are only ever called at "/" here, so the path needs no
// normalising.
func canonicalURI(u *url.URL, s3 bool) string {
	if u.Path == "" {
		return "/"
	}
	if s3 {
		return uriEncode(u.Path, true)
	}
	return uriEncode(u.EscapedPath(), true)
}

// encodeQuery is a query as AWS signs it: sorted, with every byte but the
// unreserved ones percent-encoded. Requests send it so, too, so what is
// signed is exactly what is sent.
func encodeQuery(v url.Values) string {
	var pairs [][2]string
	for k, vs := range v {
		for _, s := range vs {
			pairs = append(pairs, [2]string{uriEncode(k, false), uriEncode(s, false)})
		}
	}
	slices.SortFunc(pairs, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p[0] + "=" + p[1]
	}
	return strings.Join(out, "&")
}

func uriEncode(s string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && keepSlash {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}
