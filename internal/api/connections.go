package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// Connectors, called connections in the API, hold an organisation's
// credentials for a service, so only its administrators create and change
// them. Everyone sees their names, to connect their agents. A connector may
// instead let each person sign in with their own account: then its
// credentials are only the OAuth client's, and everyone may sign in.

type connectionView struct {
	store.Connection
	SecretsSet []string `json:"secrets_set"`
	WebhookURL string   `json:"webhook_url,omitempty"`
	Bindings   int      `json:"bindings"`
	// Account is the caller's own sign-in, to a connector people sign in to.
	Account *store.Account `json:"account,omitempty"`
}

func (s *Server) view(c store.Connection, bindings int) connectionView {
	v := connectionView{Connection: c, SecretsSet: []string{}, Bindings: bindings}
	if secrets, err := s.disp.OpenSecrets(c); err == nil {
		for k, val := range secrets {
			if val != "" {
				v.SecretsSet = append(v.SecretsSet, k)
			}
		}
		slices.Sort(v.SecretsSet)
	}
	if d, ok := connectors.Describe(connectors.Kind(c.Kind)); ok && d.Chat {
		v.WebhookURL = s.disp.WebhookURL(c.ID)
	}
	return v
}

// connectionKinds lists the kinds, and the address a connector people sign
// in to registers with the service.
func (s *Server) connectionKinds(w http.ResponseWriter, _ *http.Request, _ Principal) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data": connectors.Descriptors(), "callback_url": s.disp.AccountCallbackURL(),
	})
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request, p Principal) {
	list, err := s.st.Connections(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	counts, err := s.st.BindingCounts(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	accounts, err := s.st.UserAccounts(r.Context(), p.OrgID, p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	mine := make(map[string]*store.Account, len(accounts))
	for i := range accounts {
		mine[accounts[i].ConnectionID] = &accounts[i]
	}
	out := make([]connectionView, 0, len(list))
	for _, c := range list {
		v := s.view(c, counts[c.ID])
		if c.PerUser {
			v.Account = mine[c.ID]
		}
		out = append(out, v)
	}
	res := map[string]any{"data": out}
	// How the caller's last sign-in to a connector went, shown once.
	var signIn signInResult
	if s.readSealed(r, signInResultCookie, &signIn) {
		s.clearCookie(w, signInResultCookie, signInResultPath)
		res["sign_in_result"] = signIn
	}
	writeJSON(w, http.StatusOK, res)
}

// getConnection is one connector, with where agents use it. Someone else's
// workspace is left out, as it is from the workspaces list.
func (s *Server) getConnection(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	ctx := r.Context()
	all, err := s.st.ConnectionBindings(ctx, c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	bindings := make([]store.Binding, 0, len(all))
	for _, b := range all {
		if b.AgentKind == store.KindAgent || b.AgentOwnerID == p.UserID || p.CanAdmin() {
			bindings = append(bindings, b)
		}
	}
	v := s.view(c, len(all))
	if c.PerUser {
		acc, err := s.st.Account(ctx, c.ID, p.UserID)
		switch {
		case err == nil:
			v.Account = &acc
		case !errors.Is(err, store.ErrNotFound):
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, struct {
		connectionView
		Uses []store.Binding `json:"uses"`
	}{v, bindings})
}

type connectionInput struct {
	Kind    string            `json:"kind"`
	Name    *string           `json:"name"`
	Config  map[string]string `json:"config"`
	Secrets map[string]string `json:"secrets"`
	// PerUser lets each person sign in with their own account. It is
	// chosen when the connector is added, and kept.
	PerUser *bool `json:"per_user"`
}

// movedEndpoint returns the label of an endpoint field the form changes,
// such as a server's address, or "" if none.
func movedEndpoint(fields []connectors.Field, config, in map[string]string) string {
	moved := ""
	for _, f := range fields {
		if v, ok := in[f.Name]; ok && f.Endpoint && strings.TrimSpace(v) != config[f.Name] {
			moved = f.Label
		}
	}
	return moved
}

// fill splits a form into settings and secrets by the kind's fields, keeps
// secrets left empty, and checks the required ones. A saved secret is kept
// only while its endpoint stays the same.
func fill(d connectors.Descriptor, config, secrets, in map[string]string, inSecrets map[string]string) error {
	moved := movedEndpoint(d.Fields, config, in)
	for _, f := range d.Fields {
		if f.Secret {
			if v := strings.TrimSpace(inSecrets[f.Name]); v != "" {
				secrets[f.Name] = v
			} else if moved != "" && secrets[f.Name] != "" {
				return errors.New("enter the " + strings.ToLower(f.Label) + " again: the " + moved + " changed")
			}
			if f.Required && secrets[f.Name] == "" {
				return errors.New(f.Label + " is required")
			}
			continue
		}
		if v, ok := in[f.Name]; ok {
			config[f.Name] = strings.TrimSpace(v)
		}
		if f.Required && config[f.Name] == "" {
			return errors.New(f.Label + " is required")
		}
	}
	return nil
}

// check asks the service whether the settings work, before they are saved.
func check(ctx context.Context, c connectors.Connection) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a, ok := connectors.AdapterFor(c.Kind)
	if !ok {
		return errors.New("unknown kind of connector")
	}
	if c.PerUser {
		p, ok := connectors.PerUserFor(c.Kind)
		if !ok {
			return errors.New("this kind of connector does not let people sign in")
		}
		return p.CheckClient(ctx, c)
	}
	return a.Check(ctx, c)
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request, p Principal) {
	if !p.CanAdmin() {
		forbid(w, "only an administrator can connect a service")
		return
	}
	var in connectionInput
	if !readJSON(w, r, &in) {
		return
	}
	d, ok := connectors.Describe(connectors.Kind(in.Kind))
	if !ok {
		badRequest(w, "there is no kind of connector called "+in.Kind)
		return
	}
	perUser := in.PerUser != nil && *in.PerUser
	if perUser && d.SignIn == nil {
		badRequest(w, d.Label+" does not let people sign in with their own accounts")
		return
	}
	c := store.Connection{
		ID: id.New("con"), OrgID: p.OrgID, Kind: in.Kind, Name: d.Label, Config: map[string]string{},
		PerUser: perUser, CreatedBy: p.Email,
	}
	if err := s.prepare(r.Context(), d, &c, map[string]string{}, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.CreateConnection(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "connection.create", out.ID, map[string]string{"kind": out.Kind, "name": out.Name})
	writeJSON(w, http.StatusCreated, s.view(out, 0))
}

// prepare writes a form onto c, asks the service whether it works, and seals
// the secrets. secrets holds those already saved; a secret left empty keeps
// its value.
func (s *Server) prepare(ctx context.Context, d connectors.Descriptor, c *store.Connection, secrets map[string]string, in connectionInput) error {
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		c.Name = trim(*in.Name, 100)
	}
	// A connector people sign in to holds only its OAuth client.
	if c.PerUser && d.SignIn != nil {
		d.Fields = d.SignIn.Fields
	}
	if err := fill(d, c.Config, secrets, in.Config, in.Secrets); err != nil {
		return err
	}
	if err := check(ctx, connectors.Connection{
		ID: c.ID, Kind: d.Kind, Config: c.Config, Secrets: secrets, PerUser: c.PerUser,
	}); err != nil {
		return err
	}
	c.Secrets = s.disp.SealSecrets(c.ID, secrets)
	return nil
}

// loadConnection finds a connector in the caller's organisation. Another
// organisation's is not found, not forbidden.
func (s *Server) loadConnection(w http.ResponseWriter, r *http.Request, p Principal, id string) (store.Connection, bool) {
	c, err := s.st.Connection(r.Context(), id)
	return c, s.found(w, err, c.OrgID == p.OrgID, "connector")
}

func (s *Server) adminConnection(w http.ResponseWriter, r *http.Request, p Principal) (store.Connection, bool) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return c, false
	}
	if !p.CanAdmin() {
		forbid(w, "only an administrator can change a connector")
		return c, false
	}
	return c, true
}

func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.adminConnection(w, r, p)
	if !ok {
		return
	}
	var in connectionInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Kind != "" && in.Kind != c.Kind {
		badRequest(w, "a connector keeps its kind; create a new one")
		return
	}
	if in.PerUser != nil && *in.PerUser != c.PerUser {
		badRequest(w, "a connector keeps whom it acts as; create a new one")
		return
	}
	d, _ := connectors.Describe(connectors.Kind(c.Kind))
	secrets, err := s.disp.OpenSecrets(c)
	if err != nil {
		// Unreadable secrets, as after a changed secret key, are replaced.
		secrets = map[string]string{}
	}
	fields := d.Fields
	if c.PerUser && d.SignIn != nil {
		fields = d.SignIn.Fields
	}
	moved := movedEndpoint(fields, c.Config, in.Config)
	if err := s.prepare(r.Context(), d, &c, secrets, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	// People's tokens were given for the old server. Forgotten before the
	// new one is saved, so no call sends them there meanwhile.
	if c.PerUser && moved != "" {
		n, err := s.disp.ForgetSignIns(r.Context(), c.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.record(r.Context(), p, "connection.sign_ins_forgotten", c.ID, map[string]string{
			"name": c.Name, "count": strconv.FormatInt(n, 10),
		})
	}
	out, err := s.st.UpdateConnection(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "connection.update", out.ID, map[string]string{"name": out.Name})
	counts, _ := s.st.BindingCounts(r.Context(), p.OrgID)
	v := s.view(out, counts[out.ID])
	if a, err := s.st.Account(r.Context(), out.ID, p.UserID); err == nil && out.PerUser {
		v.Account = &a
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.adminConnection(w, r, p)
	if !ok {
		return
	}
	if err := s.st.DeleteConnection(r.Context(), c.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "connection.delete", c.ID, map[string]string{"kind": c.Kind, "name": c.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkConnection(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.adminConnection(w, r, p)
	if !ok {
		return
	}
	conn, err := s.disp.Conn(c)
	if err == nil {
		err = check(r.Context(), conn)
	}
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// connectionFolders lists a folder, for choosing one to connect an agent to,
// as the caller when people sign in to the connector. A member sees folder
// names only: the connection may reach files they could not open
// themselves. Their agents can still read those files once connected to a
// folder; docs/connectors.md says so.
func (s *Server) connectionFolders(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	adapter, ok := connectors.FilesFor(connectors.Kind(c.Kind))
	if !ok {
		badRequest(w, c.Name+" holds no files")
		return
	}
	conn, err := s.disp.ConnFor(r.Context(), c, p.UserID)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	files, err := adapter.List(r.Context(), conn, r.URL.Query().Get("parent"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	out := []connectors.File{}
	for _, f := range files {
		if f.Folder || p.CanAdmin() {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
