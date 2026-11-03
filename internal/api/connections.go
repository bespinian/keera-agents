package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// Connectors, called connections in the API, hold an organisation's
// credentials for a service, so only its administrators create and change
// them. Everyone sees their names, to connect their agents.

type connectionView struct {
	store.Connection
	SecretsSet []string `json:"secrets_set"`
	WebhookURL string   `json:"webhook_url,omitempty"`
	Bindings   int      `json:"bindings"`
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
	if d, ok := channels.Describe(channels.Kind(c.Kind)); ok && d.Chat {
		v.WebhookURL = s.disp.WebhookURL(c.ID)
	}
	return v
}

func (s *Server) connectionKinds(w http.ResponseWriter, _ *http.Request, _ Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"data": channels.Descriptors()})
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
	out := make([]connectionView, 0, len(list))
	for _, c := range list {
		out = append(out, s.view(c, counts[c.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type connectionInput struct {
	Kind    string            `json:"kind"`
	Name    *string           `json:"name"`
	Config  map[string]string `json:"config"`
	Secrets map[string]string `json:"secrets"`
}

// fill splits a form into settings and secrets by the kind's fields, keeps
// secrets left empty, and checks the required ones. A saved secret is kept
// only while its endpoint stays the same.
func fill(d channels.Descriptor, config, secrets, in map[string]string, inSecrets map[string]string) error {
	moved := ""
	for _, f := range d.Fields {
		if v, ok := in[f.Name]; ok && f.Endpoint && strings.TrimSpace(v) != config[f.Name] {
			moved = f.Label
		}
	}
	for _, f := range d.Fields {
		if f.Secret {
			if v := strings.TrimSpace(inSecrets[f.Name]); v != "" {
				secrets[f.Name] = v
			} else if moved != "" && secrets[f.Name] != "" {
				return errors.New("enter the " + strings.ToLower(f.Label) + " again: it is sent to the new " + moved)
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
func check(ctx context.Context, c channels.Connection) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a, ok := channels.AdapterFor(c.Kind)
	if !ok {
		return errors.New("unknown kind of connector")
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
	d, ok := channels.Describe(channels.Kind(in.Kind))
	if !ok {
		badRequest(w, "there is no kind of connector called "+in.Kind)
		return
	}
	c := store.Connection{
		ID: id.New("con"), OrgID: p.OrgID, Kind: in.Kind, Name: d.Label, Config: map[string]string{},
		CreatedBy: p.Email,
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
func (s *Server) prepare(ctx context.Context, d channels.Descriptor, c *store.Connection, secrets map[string]string, in connectionInput) error {
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		c.Name = trim(*in.Name, 100)
	}
	if err := fill(d, c.Config, secrets, in.Config, in.Secrets); err != nil {
		return err
	}
	if err := check(ctx, channels.Connection{
		ID: c.ID, Kind: d.Kind, Config: c.Config, Secrets: secrets,
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
	d, _ := channels.Describe(channels.Kind(c.Kind))
	secrets, err := s.disp.OpenSecrets(c)
	if err != nil {
		// Unreadable secrets, as after a changed secret key, are replaced.
		secrets = map[string]string{}
	}
	if err := s.prepare(r.Context(), d, &c, secrets, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.UpdateConnection(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "connection.update", out.ID, map[string]string{"name": out.Name})
	counts, _ := s.st.BindingCounts(r.Context(), p.OrgID)
	writeJSON(w, http.StatusOK, s.view(out, counts[out.ID]))
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

// connectionFolders lists a folder, for choosing one to connect an agent to.
// A member sees folder names only: the connection may reach files they
// could not open themselves.
func (s *Server) connectionFolders(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadConnection(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	adapter, ok := channels.FilesFor(channels.Kind(c.Kind))
	if !ok {
		badRequest(w, c.Name+" holds no files")
		return
	}
	conn, err := s.disp.Conn(c)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	files, err := adapter.List(r.Context(), conn, r.URL.Query().Get("parent"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	out := []channels.File{}
	for _, f := range files {
		if f.Folder || p.CanAdmin() {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
