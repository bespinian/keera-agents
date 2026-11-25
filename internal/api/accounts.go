package api

import (
	"errors"
	"net/http"

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
	pending
	ConnectionID string `json:"c"`
	UserID       string `json:"u"`
}

type signInResult struct {
	ConnectionID string `json:"connection_id"`
	Error        string `json:"error,omitempty"`
}

// startSignIn answers where the caller's browser goes to sign in to a
// connector. Anyone in the organisation may: they sign in as themselves.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request, p Principal) error {
	c, err := s.loadConnection(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	pending := pendingSignIn{pending: newPending(), ConnectionID: c.ID, UserID: p.UserID}
	u, err := s.disp.SignInURL(c, pending.State, challenge(pending.Verifier))
	if err != nil {
		return badRequest(err.Error())
	}
	if err := s.setSealed(w, signInCookie, signInPath, pending, loginTTL); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// signInCallback is where the service sends the browser back.
func (s *Server) signInCallback(w http.ResponseWriter, r *http.Request) {
	var pending pendingSignIn
	ok := s.readSealed(r, signInCookie, &pending)
	s.clearCookie(w, signInCookie, signInPath)
	q := r.URL.Query()
	msg, started := pending.check(ok, q)
	// Only a return the state vouches for names the connector or shows the
	// service's error.
	if started && q.Get("error") != "" {
		msg = "the service did not sign you in (" + trim(q.Get("error"), 60) + ")"
	}
	if msg != "" {
		id := ""
		if started {
			id = pending.ConnectionID
		}
		s.signedInTo(w, r, id, msg)
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
	if err != nil || (c.OrgID != me.OrgID && !me.Unrestricted) {
		s.signedInTo(w, r, "", "the connector is gone, or belongs to another of your organisations")
		return
	}
	// An operator may have started in any organisation: the connector's.
	me.OrgID = c.OrgID
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
		s.answer(w, err)
		return
	}
	http.Redirect(w, r, "/connectors", http.StatusFound)
}

// disconnectAccount forgets the caller's own sign-in to a connector.
func (s *Server) disconnectAccount(w http.ResponseWriter, r *http.Request, p Principal) error {
	c, err := s.loadConnection(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	err = s.disp.Disconnect(r.Context(), c.ID, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return refuse(http.StatusNotFound, "not_found", "you have not connected an account to "+c.Name)
	}
	if err != nil {
		return err
	}
	s.record(r.Context(), p, "account.disconnect", c.ID, map[string]string{"connection": c.Name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}
