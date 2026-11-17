package api

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
)

func TestFillKeepsSecretsOnlyForTheSameEndpoint(t *testing.T) {
	d := connectors.Descriptor{Fields: []connectors.Field{
		{Name: "token", Label: "Access token", Secret: true, Required: true},
		{Name: "base_url", Label: "GitLab URL", Endpoint: true},
		{Name: "note", Label: "Note"},
	}}
	saved := func() (map[string]string, map[string]string) {
		return map[string]string{"base_url": "https://gitlab.example.com", "note": "a"},
			map[string]string{"token": "glpat-saved"}
	}

	for _, tc := range []struct {
		name      string
		in, inSec map[string]string
		wantErr   string
		wantToken string
	}{
		{name: "other settings keep the token", in: map[string]string{"note": "b"}, wantToken: "glpat-saved"},
		{name: "the same URL keeps the token", in: map[string]string{"base_url": " https://gitlab.example.com "}, wantToken: "glpat-saved"},
		{name: "a new URL needs the token again", in: map[string]string{"base_url": "https://evil.example"}, wantErr: "enter the access token again"},
		{name: "clearing the URL needs the token again", in: map[string]string{"base_url": ""}, wantErr: "enter the access token again"},
		{
			name: "a new URL with a new token", in: map[string]string{"base_url": "https://gitlab2.example.com"},
			inSec: map[string]string{"token": "glpat-new"}, wantToken: "glpat-new",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, secrets := saved()
			err := fill(d, config, secrets, tc.in, tc.inSec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if secrets["token"] != tc.wantToken {
				t.Errorf("token = %q, want %q", secrets["token"], tc.wantToken)
			}
		})
	}

	// A new connector has nothing saved, so naming its URL is fine.
	if err := fill(d, map[string]string{}, map[string]string{},
		map[string]string{"base_url": "https://gitlab.example.com"}, map[string]string{"token": "glpat-x"}); err != nil {
		t.Fatal(err)
	}
}
