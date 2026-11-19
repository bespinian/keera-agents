package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// writeConfigs points the workspace's programs at the bridge and lets the
// owner's SSH keys in. It writes only its own entries, so what the owner
// changed in the same files stays. A failure is logged: the workspace is
// still of use without it. starting is true before the programs run.
func (r *runner) writeConfigs(ctx context.Context, starting bool) {
	home := r.cfg.Workspace
	if err := r.writeSSHConfig(home); err != nil {
		r.log.Warn("workspace: ssh will not let anybody in", "error", err)
	}
	models, err := r.listModels(ctx)
	if err != nil {
		r.log.Warn("workspace: could not list the gateway's models for the coding agent", "error", err)
	}
	mcp := bridgeURL + "/mcp"
	for _, f := range []struct {
		path string
		edit func(map[string]any)
	}{
		{filepath.Join(home, ".pi", "agent", "models.json"), func(m map[string]any) {
			if models != nil {
				object(m, "providers")["keera"] = piProvider(models)
			}
		}},
		{filepath.Join(home, ".pi", "agent", "settings.json"), func(m map[string]any) {
			// Only a first choice: the owner picks another with /model.
			if _, ok := m["defaultModel"]; !ok {
				m["defaultProvider"], m["defaultModel"] = "keera", r.snapshot().Agent.Model
			}
		}},
		// Pi's own MCP client reads this.
		{filepath.Join(home, ".pi", "agent", "mcp.json"), func(m map[string]any) {
			object(m, "mcpServers")["keera"] = map[string]any{
				"url": mcp, "description": "This workspace's connectors and the organisation's MCP servers",
			}
		}},
		// Defaults only: what the owner set stays. The owner's own volume is
		// all code-server opens, so Restricted Mode would only get in the
		// way. The theme follows the system's, as the panel's does at first.
		// No welcome page or walkthroughs at start: they are mostly noise.
		{filepath.Join(codeServerUser(home), "settings.json"), func(m map[string]any) {
			for k, v := range map[string]any{
				"security.workspace.trust.enabled":                 false,
				"window.autoDetectColorScheme":                     true,
				"workbench.startupEditor":                          "none",
				"workbench.welcomePage.walkthroughs.openOnInstall": false,
			} {
				if _, ok := m[k]; !ok {
					m[k] = v
				}
			}
		}},
		// Cline reads its servers from the extension's storage, and from
		// ~/.cline since version 4.
		{clineMCPSettings(home), clineMCP},
		{filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json"), clineMCP},
	} {
		if err := mergeJSON(f.path, 0o644, f.edit); err != nil {
			r.log.Warn("workspace: could not write a configuration file", "path", f.path, "error", err)
		}
	}
	// Cline keeps its state in memory and writes the whole file on each
	// change. Written while it runs, it could lose ours or we its.
	if starting {
		if err := writeClineProvider(home, r.snapshot().Agent.Model, models); err != nil {
			r.log.Warn("workspace: could not set Cline's model", "error", err)
		}
	}
}

// writeClineProvider points Cline at the bridge, with the workspace's model.
// Only a first choice, as for pi: once a provider is set, it is the owner's.
// Cline reads these files since version 4, and fills its providers.json from
// them at start.
func writeClineProvider(home, model string, models []gatewayModel) error {
	if model == "" {
		return nil
	}
	dir := filepath.Join(home, ".cline", "data")
	set := false
	err := mergeJSON(filepath.Join(dir, "globalState.json"), 0o644, func(m map[string]any) {
		if _, ok := m["actModeApiProvider"]; ok {
			return
		}
		set = true
		m["openAiBaseUrl"] = bridgeURL + "/v1"
		m["openAiHeaders"] = map[string]any{"X-Keera-Client": "cline"}
		for _, mode := range []string{"actMode", "planMode"} {
			m[mode+"ApiProvider"] = "openai"
			m[mode+"OpenAiModelId"] = model
			if info := clineModelInfo(model, models); info != nil {
				m[mode+"OpenAiModelInfo"] = info
			}
		}
		// The welcome view asks to sign in to Cline, which is not needed.
		m["welcomeViewCompleted"] = true
	})
	if err != nil || !set {
		return err
	}
	return mergeJSON(filepath.Join(dir, "secrets.json"), 0o600, func(m map[string]any) {
		if _, ok := m["openAiApiKey"]; !ok {
			// Any key does: the bridge adds the workspace's.
			m["openAiApiKey"] = "keera"
		}
	})
}

// clineModelInfo is Cline's description of the model, with the context
// window the gateway knows. The rest are Cline's own defaults.
func clineModelInfo(model string, models []gatewayModel) map[string]any {
	for _, g := range models {
		if g.ID == model && g.MaxContext > 0 {
			return map[string]any{
				"contextWindow": g.MaxContext, "maxTokens": -1, "supportsImages": true,
				"supportsPromptCache": false, "inputPrice": 0, "outputPrice": 0, "temperature": 0,
			}
		}
	}
	return nil
}

func clineMCP(m map[string]any) {
	object(m, "mcpServers")["keera"] = map[string]any{
		"type": "streamableHttp", "url": bridgeURL + "/mcp", "disabled": false,
	}
}

// codeServerUser is code-server's user directory: its settings and the
// extensions' storage.
func codeServerUser(home string) string {
	return filepath.Join(home, ".local", "share", "code-server", "User")
}

// clineMCPSettings is where Cline keeps its MCP servers inside code-server.
func clineMCPSettings(home string) string {
	return filepath.Join(codeServerUser(home), "globalStorage",
		"saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
}

// gatewayModel is one entry of the gateway's /v1/models: the models and
// routers the key may use.
type gatewayModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	MaxContext  int    `json:"max_context_tokens"`
}

func (r *runner) listModels(ctx context.Context) ([]gatewayModel, error) {
	boot := r.snapshot()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(boot.Gateway.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey())
	resp, err := r.gw.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, readError("the gateway", resp)
	}
	var list struct {
		Data []gatewayModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list.Data, nil
}

// piProvider is pi's provider for the bridge, in the shape of the gateway's
// own `keera connect pi`.
func piProvider(models []gatewayModel) map[string]any {
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		e := map[string]any{"id": m.ID, "name": m.DisplayName}
		if m.MaxContext > 0 {
			e["contextWindow"] = m.MaxContext
		}
		entries = append(entries, e)
	}
	return map[string]any{
		"baseUrl": bridgeURL + "/v1",
		"api":     "openai-completions",
		// Any key does: the bridge adds the workspace's.
		"apiKey":  "keera",
		"headers": map[string]any{"X-Keera-Client": "pi"},
		"models":  entries,
	}
}

// object returns m[key] as an object, making it if it is missing or not one.
func object(m map[string]any, key string) map[string]any {
	c, ok := m[key].(map[string]any)
	if !ok {
		c = map[string]any{}
		m[key] = c
	}
	return c
}

// mergeJSON applies edit to the JSON object in path and writes it back, if
// it changed anything. A file that is not a JSON object is the owner's and
// is left alone.
func mergeJSON(path string, mode os.FileMode, edit func(map[string]any)) error {
	m := map[string]any{}
	raw, err := os.ReadFile(path)
	exists := err == nil
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return errors.New("it is not a JSON object, so it was left as it is")
		}
	}
	before, err := json.Marshal(m)
	if err != nil {
		return err
	}
	edit(m)
	if after, err := json.Marshal(m); err != nil || (exists && bytes.Equal(before, after)) {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(out, '\n'), mode)
}

// writeFile replaces path by renaming, so a reader never sees half a file.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".keera-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeSSHConfig sets up sshd to run as the workspace's user, on the
// loopback, with the owner's keys only. Its host key is kept in the
// workspace, so ssh knows it again after a restart.
func (r *runner) writeSSHConfig(home string) error {
	dir := filepath.Join(home, ".keera", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hostKey := filepath.Join(dir, "host_ed25519")
	if _, err := os.Stat(hostKey); errors.Is(err, fs.ErrNotExist) {
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "keera-workspace",
			"-f", hostKey).CombinedOutput()
		if err != nil {
			return errors.New("making the host key: " + strings.TrimSpace(string(out)))
		}
	}
	boot := r.snapshot()
	var keys []string
	if boot.Workspace != nil {
		keys = boot.Workspace.SSHKeys
	}
	if err := writeFile(filepath.Join(dir, "authorized_keys"), []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	// Not root, so sshd neither changes user nor uses PAM. StrictModes would
	// refuse a volume the cluster made group-writable, and only one user
	// lives here.
	conf := strings.Join([]string{
		"ListenAddress 127.0.0.1:" + strconv.Itoa(sshPort),
		"HostKey " + hostKey,
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"),
		"PidFile /tmp/sshd.pid",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitRootLogin no",
		"UsePAM no",
		"StrictModes no",
		"AllowTcpForwarding yes",
		"Subsystem sftp internal-sftp",
		"",
	}, "\n")
	return writeFile(filepath.Join(dir, "sshd_config"), []byte(conf), 0o600)
}
