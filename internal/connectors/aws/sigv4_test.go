package aws

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The cases come from AWS's Signature Version 4 test suite, which signs for
// the service "service" with this key, region and time.
func TestSignTestVectors(t *testing.T) {
	key := creds{id: "AKIDEXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	cases := []struct {
		name, method, url, contentType, body string
		token                                string
		want                                 string
	}{
		{
			name: "get-vanilla", method: http.MethodGet, url: "https://example.amazonaws.com/",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name: "get-vanilla-query-order-key-case", method: http.MethodGet,
			url: "https://example.amazonaws.com/?Param2=value2&Param1=value1",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			name: "post-vanilla", method: http.MethodPost, url: "https://example.amazonaws.com/",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		},
		{
			name: "post-x-www-form-urlencoded", method: http.MethodPost, url: "https://example.amazonaws.com/",
			contentType: "application/x-www-form-urlencoded", body: "Param1=value1",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
		{
			name: "post-sts-header-before", method: http.MethodPost, url: "https://example.amazonaws.com/",
			token: "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA==",
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, c.url, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			k := key
			k.token = c.token
			sign(req, []byte(c.body), k, "us-east-1", "service", at)
			if got := req.Header.Get("Authorization"); got != c.want {
				t.Fatalf("Authorization =\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

func TestEncode(t *testing.T) {
	q := url.Values{"b": {"2", "1"}, "a-b": {"x"}, "a": {"a b/+~"}}
	if got, want := encodeQuery(q), "a=a%20b%2F%2B~&a-b=x&b=1&b=2"; got != want {
		t.Fatalf("encodeQuery = %s, want %s", got, want)
	}
	u, _ := url.Parse("https://s3.test/bucket/a%20b/c%2Bd")
	if got := canonicalURI(u, true); got != "/bucket/a%20b/c%2Bd" {
		t.Fatalf("S3 path = %s", got)
	}
	if got := canonicalURI(u, false); got != "/bucket/a%2520b/c%252Bd" {
		t.Fatalf("other path = %s", got)
	}
}
