package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestTokens(t *testing.T) {
	var tk tokens
	ctx := context.Background()
	fetches := 0
	fetch := func(g channels.Grant) func(context.Context) (channels.Grant, error) {
		return func(context.Context) (channels.Grant, error) {
			fetches++
			return g, nil
		}
	}
	hour := time.Now().Add(time.Hour)

	// A sign-in's token is used until it nears its expiry.
	tk.set("con_1", "usr_1", channels.Grant{Refresh: "r1", Access: "a0", Expiry: hour})
	if got, _ := tk.get(ctx, "con_1", "usr_1", "r1", fetch(channels.Grant{Access: "a1", Expiry: hour})); got != "a0" || fetches != 0 {
		t.Fatalf("get = %q after %d fetches", got, fetches)
	}
	// Another sign-in, with another refresh token, replaces it.
	if got, _ := tk.get(ctx, "con_1", "usr_1", "r2", fetch(channels.Grant{Access: "a2", Expiry: hour})); got != "a2" || fetches != 1 {
		t.Fatalf("new sign-in: %q after %d fetches", got, fetches)
	}
	// A rotated refresh token keeps the token it came with.
	if got, _ := tk.get(ctx, "con_2", "usr_1", "r1", fetch(channels.Grant{Refresh: "r9", Access: "a3", Expiry: hour})); got != "a3" {
		t.Fatalf("rotated: %q", got)
	}
	if got, _ := tk.get(ctx, "con_2", "usr_1", "r9", fetch(channels.Grant{Access: "a4", Expiry: hour})); got != "a3" || fetches != 2 {
		t.Fatalf("after rotation: %q after %d fetches", got, fetches)
	}
	// A caller that read the token before it rotated gets the new access
	// token, not a refresh with the old one.
	if got, _ := tk.get(ctx, "con_2", "usr_1", "r1", fetch(channels.Grant{Access: "a5", Expiry: hour})); got != "a3" || fetches != 2 {
		t.Fatalf("read before rotation: %q after %d fetches", got, fetches)
	}
	// A token about to expire is replaced.
	tk.set("con_3", "usr_2", channels.Grant{Refresh: "r", Access: "old", Expiry: time.Now().Add(time.Minute)})
	if got, _ := tk.get(ctx, "con_3", "usr_2", "r", fetch(channels.Grant{Access: "new", Expiry: hour})); got != "new" {
		t.Fatalf("expiring: %q", got)
	}
	// Someone who left is forgotten everywhere.
	tk.forgetUser("usr_1")
	if len(tk.m) != 1 {
		t.Fatalf("after forgetUser: %v", tk.m)
	}
}
