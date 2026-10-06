package api

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// A person's SSH keys let them into their own workspaces. Each person sees
// and changes only their own.

func (s *Server) listSSHKeys(w http.ResponseWriter, r *http.Request, p Principal) {
	keys, err := s.st.SSHKeys(r.Context(), p.OrgID, p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": keys})
}

func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	line, comment, fingerprint, err := parsePublicKey(in.PublicKey)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	k, err := s.st.AddSSHKey(r.Context(), store.SSHKey{
		ID: id.New("ssh"), OrgID: p.OrgID, UserID: p.UserID,
		Name: trim(cmp.Or(strings.TrimSpace(in.Name), comment), 100), PublicKey: line, Fingerprint: fingerprint,
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "exists", "you added this key already")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "ssh_key.add", k.ID, map[string]string{"fingerprint": fingerprint})
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) deleteSSHKey(w http.ResponseWriter, r *http.Request, p Principal) {
	err := s.st.DeleteSSHKey(r.Context(), p.OrgID, p.UserID, r.PathValue("id"))
	if !s.found(w, err, true, "key") {
		return
	}
	s.record(r.Context(), p, "ssh_key.delete", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

// sshKeyTypes are the key types OpenSSH takes today. DSA is gone from it.
var sshKeyTypes = []string{
	"ssh-ed25519", "sk-ssh-ed25519@openssh.com",
	"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	"sk-ecdsa-sha2-nistp256@openssh.com", "ssh-rsa",
}

// parsePublicKey reads one public key in authorized_keys form, without
// options, and returns it as one clean line, its comment and its
// fingerprint as ssh-keygen -l prints it.
func parsePublicKey(s string) (line, comment, fingerprint string, err error) {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) < 2 || strings.ContainsAny(s, "\r\n") {
		return "", "", "", errors.New("paste one public key, such as the line in ~/.ssh/id_ed25519.pub")
	}
	typ := fields[0]
	if !slices.Contains(sshKeyTypes, typ) {
		return "", "", "", errors.New("that is not a public key OpenSSH takes; paste the .pub file's line")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || !blobHasType(blob, typ) {
		return "", "", "", errors.New("the key is damaged; paste the .pub file's line as it is")
	}
	comment = trim(strings.Join(fields[2:], " "), 100)
	sum := sha256.Sum256(blob)
	line = typ + " " + fields[1]
	if comment != "" {
		line += " " + comment
	}
	return line, comment, "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// blobHasType checks that a key's blob starts with its own type, as every
// OpenSSH public key does.
func blobHasType(blob []byte, typ string) bool {
	if len(blob) < 4 {
		return false
	}
	n := binary.BigEndian.Uint32(blob)
	return uint64(n) == uint64(len(typ)) && len(blob) >= 4+len(typ) && bytes.Equal(blob[4:4+len(typ)], []byte(typ))
}
