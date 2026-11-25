package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/billing"
)

func TestBillingIsForAdministratorsOnBillingDeployments(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name   string
		p      Principal
		biller *billing.Biller
		status int
	}{
		{"a member", member, billing.New(nil, nil, billing.Prices{CPUHour: 1}, nil), http.StatusForbidden},
		{"no billing", admin, nil, http.StatusNotFound},
		{"no prices", admin, billing.New(nil, nil, billing.Prices{}, nil), http.StatusNotFound},
	} {
		s.opts.Billing = tc.biller
		w := httptest.NewRecorder()
		s.answer(w, s.billing(w, httptest.NewRequest("GET", "/api/v1/billing", nil), tc.p))
		if w.Code != tc.status {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.status)
		}
	}
}

func TestARefusalForCreditIs402(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	s.answer(w, runtimeErr(&billing.Refused{Code: "credit_exhausted", Message: "no credit"}))
	var body struct {
		Error struct{ Code, Message string }
	}
	_ = json.NewDecoder(w.Body).Decode(&body)
	if w.Code != http.StatusPaymentRequired || body.Error.Code != "credit_exhausted" || body.Error.Message != "no credit" {
		t.Errorf("%d %+v", w.Code, body)
	}
	w = httptest.NewRecorder()
	s.answer(w, runtimeErr(errors.New("no node")))
	if w.Code != http.StatusBadGateway {
		t.Errorf("any other failure: %d", w.Code)
	}
}

func TestParseMonth(t *testing.T) {
	now := time.Date(2026, 11, 17, 23, 30, 0, 0, time.FixedZone("CET", 3600))
	if got, err := parseMonth("", now); err != nil || !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("empty = %s, %v", got, err)
	}
	if got, err := parseMonth("2026-09", now); err != nil || !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("2026-09 = %s, %v", got, err)
	}
	if _, err := parseMonth("September", now); err == nil {
		t.Error("a month by name was taken")
	}
}
