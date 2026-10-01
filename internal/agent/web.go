package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/version"
)

// userAgent names the runtime to the sites it fetches and to the gateway,
// which lists its requests as Keera Agent rather than as a Go client.
var userAgent = "keera-agent/" + version.String()

const (
	webTimeout = 30 * time.Second
	// maxWebBody is the most read from a page; maxWebText the most returned.
	maxWebBody = 2 << 20
	maxWebText = 32 << 10
)

func webTool(client *http.Client) tool {
	return tool{
		name: "web_fetch",
		description: fmt.Sprintf("Fetch a web page or file over HTTP(S) with GET and return its text "+
			"(HTML is reduced to plain text; at most %d KiB). Outbound access may be limited by policy.",
			maxWebText>>10),
		parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"url":{"type":"string","description":"An http or https URL."}},"required":["url"]}`),
		summary: summarise("web_fetch", "url"),
		run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				URL string `json:"url"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			return fetch(ctx, client, a.URL)
		},
	}
}

func fetch(ctx context.Context, client *http.Client, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the url must be an http or https address")
	}
	ctx, cancel := context.WithTimeout(ctx, webTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html, text/plain, application/json, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %v", u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebBody))
	if err != nil {
		return "", fmt.Errorf("reading %s: %v", u.Redacted(), err)
	}

	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	text := string(body)
	switch {
	case ctype == "text/html" || ctype == "application/xhtml+xml" ||
		(ctype == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")):
		text = htmlText(text)
	case isBinary(body):
		return fmt.Sprintf("HTTP %d: %s is binary (%s, %s); it cannot be shown as text",
			resp.StatusCode, u.Redacted(), ctype, size(int64(len(body)))), nil
	}
	out := fmt.Sprintf("HTTP %d %s\n\n", resp.StatusCode, ctype)
	if len(text) > maxWebText {
		text = truncate(text, maxWebText) + "\n[truncated]"
	}
	return out + text, nil
}

var (
	reHidden = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b.*?</(script|style|noscript|svg|head)\s*>`)
	reBlock  = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/section|/article|/header|/footer|/table)\b[^>]*>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank  = regexp.MustCompile(`\n\s*\n+`)
	reSpaces = regexp.MustCompile(`[ \t\r\f\v]+`)
)

// htmlText reduces HTML to its text, crudely: enough for a model to read a
// page, not to render it.
func htmlText(s string) string {
	s = reHidden.ReplaceAllString(s, " ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(reBlank.ReplaceAllString(s, "\n\n"))
}
