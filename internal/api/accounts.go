package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/bespinian/keera-agents/internal/store"
)

// People sign in to a connector themselves, so agents act as them there. It
// is OAuth with an authorization code and PKCE, as signing in to Keera
// Agents is: the browser goes to the service with a challenge and a state
// kept in a sealed cookie, and comes back to dispatch.AccountCallbackPath
// with a code, which the control plane redeems. The tokens never reach the
// browser.

const (
	// signInCookie is held while the person is at the service.
	signInCookie = "keera_agents_account"
	signInPath   = "/auth/connector/"
	// signInResultCookie says how it went, until the connector list shows
	// it, so no message travels in a URL.
	signInResultCookie = "keera_agents_account_result"
	signInResultPath   = "/api/v1/connections"
)

type pendingSignIn struct {
	State        string `json:"s"`
	Verifier     string `json:"v"`
	ConnectionID string `json:"c"`
	UserID       string `json:"u"`
	Expires      int64  `json:"x"`
}

type signInResult struct {
	ConnectionID string `json:"connection_id"`
	Error        string `json:"error,omitempty"`
}

// startSignIn answers where the caller's browser goes to sign in to a
// connector. Anyone in the organisation may: they sign in as themselves.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	pending := pendingSignIn{
		State: randomToken(), Verifier: randomToken(), ConnectionID: c.ID, UserID: p.UserID,
		Expires: time.Now().Add(loginTTL).Unix(),
	}
	u, err := s.disp.SignInURL(c, pending.State, challenge(pending.Verifier))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := s.setSealed(w, signInCookie, signInPath, pending, loginTTL); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// signInCallback is where the service sends the browser back.
func (s *Server) signInCallback(w http.ResponseWriter, r *http.Request) {
	var pending pendingSignIn
	ok := s.readSealed(r, signInCookie, &pending)
	s.clearCookie(w, signInCookie, signInPath)
	q := r.URL.Query()
	switch {
	case !ok || time.Now().Unix() > pending.Expires:
		s.signedInTo(w, r, "", "this sign-in took too long, or was started in another browser; try again")
		return
	// The state stops a link from connecting someone else's account.
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(pending.State)) != 1:
		s.signedInTo(w, r, "", "this sign-in was not the one this browser started; try again")
		return
	case q.Get("error") != "":
		s.signedInTo(w, r, pending.ConnectionID, "the service did not sign you in ("+trim(q.Get("error"), 60)+")")
		return
	case q.Get("code") == "":
		s.signedInTo(w, r, pending.ConnectionID, "the sign-in came back without a code; try again")
		return
	}
	// The person who started must still be the one signed in here, so an
	// account never lands on somebody else's sign-in.
	sess, ok := s.readSession(r)
	if !ok {
		s.signedInTo(w, r, pending.ConnectionID, "your sign-in to Keera Agents has ended; sign in and try again")
		return
	}
	me, err := s.whoIs(r.Context(), sess.Token)
	if err != nil || me.UserID != pending.UserID {
		s.signedInTo(w, r, pending.ConnectionID, "sign in to Keera Agents as the person who started this, and try again")
		return
	}
	c, err := s.st.Connection(r.Context(), pending.ConnectionID)
	if err != nil || c.OrgID != me.OrgID {
		s.signedInTo(w, r, "", "the connector is gone, or belongs to another of your organisations")
		return
	}
	a, err := s.disp.Connect(r.Context(), c, me.UserID, q.Get("code"), pending.Verifier)
	if err != nil {
		s.signedInTo(w, r, c.ID, err.Error())
		return
	}
	s.record(r.Context(), Principal{Me: me}, "account.connect", c.ID,
		map[string]string{"connection": c.Name, "account": a.Account})
	s.signedInTo(w, r, c.ID, "")
}

// signedInTo goes back to the connectors with how the sign-in went.
func (s *Server) signedInTo(w http.ResponseWriter, r *http.Request, connectionID, msg string) {
	if err := s.setSealed(w, signInResultCookie, signInResultPath,
		signInResult{ConnectionID: connectionID, Error: msg}, errorTTL); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/connectors", http.StatusFound)
}

// disconnectAccount forgets the caller's own sign-in to a connector.
func (s *Server) disconnectAccount(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	err := s.disp.Disconnect(r.Context(), c.ID, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "you have not connected an account to "+c.Name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "account.disconnect", c.ID, map[string]string{"connection": c.Name})
	w.WriteHeader(http.StatusNoContent)
}
