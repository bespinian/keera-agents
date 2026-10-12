package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// The podman driver runs agents on one host, for development. It is the
// weaker driver: one host, whatever OCI runtime the host has (usually crun),
// no network policy, and an unsized volume.
//
// An agent is one container and one named volume. Suspend stops the
// container and Resume starts it again; the volume stays. Create always
// replaces the container, since a container's environment, and so its
// runtime token, is fixed once it exists.

type PodmanOptions struct {
	Binary string
	// Network empty is podman's default network.
	Network string
	// Runtimes maps a tier to an OCI runtime: "crun" or "runc" for standard,
	// "runsc" for isolated, "krun" for vm. A tier other than standard with no
	// entry is refused.
	Runtimes map[Isolation]string
}

type Podman struct {
	opts PodmanOptions
}

// NewPodman runs `podman version` as a check that podman is there.
func NewPodman(ctx context.Context, opts PodmanOptions) (*Podman, error) {
	p := &Podman{opts: opts}
	if _, err := p.run(ctx, "version", "--format", "{{.Client.Version}}"); err != nil {
		return nil, fmt.Errorf("running %s: %w; the podman driver needs it on PATH", opts.Binary, err)
	}
	return p, nil
}

func (p *Podman) Name() string { return "podman" }

func (p *Podman) Tiers() []Isolation { return tiersFor(p.opts.Runtimes) }

// containerName and volumeName depend only on the agent's id, so a restarted
// control plane finds what it left running.
func containerName(agentID string) string { return objectName(agentID) }
func volumeName(agentID string) string    { return objectName(agentID) + "-workspace" }

func (p *Podman) Create(ctx context.Context, spec Spec) (Status, error) {
	if err := checkSpec(spec); err != nil {
		return Status{}, err
	}
	rt, err := runtimeFor(spec.Class, p.opts.Runtimes, p.Name())
	if err != nil {
		return Status{}, err
	}
	args, env := p.runArgs(spec, rt)

	// Every start brings a new runtime token, so a container that is there is
	// always replaced. The volume is named, so removing the container keeps
	// it, and the new container mounts it again.
	if err := p.removeContainer(ctx, containerName(spec.AgentID)); err != nil {
		return Status{}, err
	}

	if _, err := p.runEnv(ctx, env, args...); err != nil {
		return Status{}, err
	}
	return p.Status(ctx, spec.AgentID)
}

// runArgs is the `podman run` command line and podman's environment.
func (p *Podman) runArgs(spec Spec, rt string) (args, env []string) {
	args = []string{"run", "--detach", "--name", containerName(spec.AgentID)}
	if rt != "" {
		args = append(args, "--runtime", rt)
	}
	if p.opts.Network != "" {
		args = append(args, "--network", p.opts.Network)
	}
	// A workspace's front must be reachable from keera-agents. On a network
	// of its own, keera-agents is in a container on it too and uses the
	// name. Otherwise keera-agents runs on the host: a port on the loopback
	// only, chosen by podman.
	if spec.Workspace && p.opts.Network == "" {
		args = append(args, "--publish", "127.0.0.1::"+strconv.Itoa(protocol.FrontPort))
	}
	// Labels are the only record of an agent outside the database.
	labels := labelsFor(spec)
	maps.Copy(labels, annotationsFor(spec))
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", name+"="+labels[name])
	}
	// checkSpec has refused a CPU or memory that is not a positive size.
	cpu, _ := parseQuantity(spec.Class.CPU)
	mem, _ := parseQuantity(spec.Class.Memory)
	m := strconv.FormatInt(int64(mem), 10)
	// The same for swap, so the limit is the limit.
	args = append(args, "--cpus", strconv.FormatFloat(cpu, 'f', 3, 64), "--memory", m, "--memory-swap", m)
	// The same hardening as the Kubernetes driver. on-failure restarts a
	// crashed runtime, but not one stopped by Suspend. /tmp is a tmpfs, which
	// counts against the memory limit.
	args = append(args,
		"--restart", "on-failure",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--user", strconv.Itoa(agentUID)+":"+strconv.Itoa(agentUID),
		"--read-only",
		"--read-only-tmpfs=false",
		"--tmpfs", "/tmp:rw,size=1g,mode=1777,nosuid,nodev",
		"--pids-limit", strconv.Itoa(pidsLimit),
		"--volume", volumeName(spec.AgentID)+":"+protocol.Workspace,
	)
	// Every user on the host can read a process's arguments, so values go
	// through podman's environment. A name podman uses itself, such as PATH,
	// stays on the command line, or podman would run with the agent's value.
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		kv := name + "=" + spec.Env[name]
		if _, podmans := os.LookupEnv(name); podmans && !strings.HasPrefix(name, "KEERA_") {
			args = append(args, "--env", kv)
			continue
		}
		args = append(args, "--env", name)
		env = append(env, kv)
	}
	args = append(args, spec.Image)
	return args, env
}

type podmanInspect struct {
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
		Error     string `json:"Error"`
	} `json:"State"`
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
}

func (p *Podman) inspect(ctx context.Context, agentID string) (podmanInspect, error) {
	out, err := p.run(ctx, "inspect", "--type", "container", containerName(agentID))
	if err != nil {
		return podmanInspect{}, podmanNotFound(err)
	}
	var items []podmanInspect
	if err := json.Unmarshal(out, &items); err != nil {
		return podmanInspect{}, fmt.Errorf("reading the state of agent %s: %w", agentID, err)
	}
	// Podman can answer a missing container with an empty list.
	if len(items) == 0 {
		return podmanInspect{}, ErrNotFound
	}
	return items[0], nil
}

func (p *Podman) Status(ctx context.Context, agentID string) (Status, error) {
	in, err := p.inspect(ctx, agentID)
	if errors.Is(err, ErrNotFound) {
		return Status{State: Gone, Detail: "there is no container"}, nil
	}
	if err != nil {
		return Status{}, err
	}
	state, detail := podmanState(in)
	var token string
	if state == Running {
		for _, kv := range in.Config.Env {
			if v, ok := strings.CutPrefix(kv, protocol.EnvRuntimeToken+"="); ok {
				token = v
			}
		}
	}
	return Status{State: state, Detail: detail, Token: token}, nil
}

// Exit codes after `podman stop`: SIGTERM, or SIGKILL when the runtime took
// longer than the stop timeout.
const (
	exitSIGTERM = 128 + 15
	exitSIGKILL = 128 + 9
)

func podmanState(in podmanInspect) (State, string) {
	s := in.State
	switch {
	case s.Running:
		return Running, ""
	case s.Status == "created" || s.Status == "configured" || s.Status == "initialized":
		return Pending, "starting"
	case s.Status == "exited" || s.Status == "stopped":
		if s.OOMKilled {
			return Failed, "the runtime ran out of memory"
		}
		// A stop looks like this. A crash with on-failure is restarted, so a
		// container that stays down with these codes was stopped.
		if s.ExitCode == 0 || s.ExitCode == exitSIGTERM || s.ExitCode == exitSIGKILL {
			return Suspended, "stopped; its volume is kept"
		}
		detail := fmt.Sprintf("the runtime exited with status %d", s.ExitCode)
		if s.Error != "" {
			detail += ": " + s.Error
		}
		return Failed, detail
	default:
		return Pending, s.Status
	}
}

// stopTimeout gives the runtime time to post a task's last events.
const stopTimeout = "20"

func (p *Podman) Suspend(ctx context.Context, agentID string) error {
	_, err := p.run(ctx, "stop", "--time", stopTimeout, containerName(agentID))
	return podmanNotFound(err)
}

func (p *Podman) Resume(ctx context.Context, agentID string) error {
	_, err := p.run(ctx, "start", containerName(agentID))
	return podmanNotFound(err)
}

func (p *Podman) Terminate(ctx context.Context, agentID string) error {
	if err := p.removeContainer(ctx, containerName(agentID)); err != nil {
		return err
	}
	// `podman rm --volumes` leaves a named volume behind.
	if _, err := p.run(ctx, "volume", "rm", "--force", volumeName(agentID)); err != nil && !isNoSuch(err) {
		return err
	}
	return nil
}

// removeContainer counts a container already gone as removed.
func (p *Podman) removeContainer(ctx context.Context, name string) error {
	if _, err := p.run(ctx, "rm", "--force", "--time", stopTimeout, name); err != nil && !isNoSuch(err) {
		return err
	}
	return nil
}

// Logs runs `podman logs`, which prints the container's stdout and stderr in
// turn. Closing the reader stops the command.
func (p *Podman) Logs(ctx context.Context, agentID string, follow bool) (io.ReadCloser, error) {
	if _, err := p.inspect(ctx, agentID); err != nil {
		return nil, err
	}
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, p.opts.Binary, append(args, containerName(agentID))...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("reading the output of agent %s: %w", agentID, err)
	}
	go func() {
		err := cmd.Wait()
		cancel()
		_ = pw.CloseWithError(err)
	}()
	return &podmanLogs{PipeReader: pr, cancel: cancel}, nil
}

// Address finds a workspace's front: by name on the podman network, or the
// loopback port podman published.
func (p *Podman) Address(ctx context.Context, agentID string) (string, error) {
	in, err := p.inspect(ctx, agentID)
	if err != nil {
		return "", err
	}
	if !in.State.Running {
		return "", ErrNotFound
	}
	if p.opts.Network != "" {
		return net.JoinHostPort(containerName(agentID), strconv.Itoa(protocol.FrontPort)), nil
	}
	out, err := p.run(ctx, "port", containerName(agentID), strconv.Itoa(protocol.FrontPort)+"/tcp")
	if err != nil {
		return "", podmanNotFound(err)
	}
	// One line per address, such as 127.0.0.1:41235.
	addr, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if addr == "" {
		return "", fmt.Errorf("agent %s publishes no port; start it again", agentID)
	}
	return addr, nil
}

type podmanLogs struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (l *podmanLogs) Close() error {
	l.cancel()
	return l.PipeReader.Close()
}

// podmanTimeout is generous because `run` may include an image pull.
const podmanTimeout = 5 * time.Minute

func (p *Podman) run(ctx context.Context, args ...string) ([]byte, error) {
	return p.runEnv(ctx, nil, args...)
}

func (p *Podman) runEnv(ctx context.Context, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, podmanTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.opts.Binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, &podmanError{Binary: p.opts.Binary, Args: args, Msg: msg, Err: err}
	}
	return stdout.Bytes(), nil
}

// podmanError carries what the command printed. It holds the arguments but
// not the environment, where the secrets are.
type podmanError struct {
	Binary string
	Args   []string
	Msg    string
	Err    error
}

func (e *podmanError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Binary, e.Args[0], e.Msg)
}

func (e *podmanError) Unwrap() error { return e.Err }

// isNoSuch matches the message, since podman's exit code 125 is shared with
// every usage error.
func isNoSuch(err error) bool {
	var pe *podmanError
	if !errors.As(err, &pe) {
		return false
	}
	msg := strings.ToLower(pe.Msg)
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such volume") ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "not found")
}

// podmanNotFound maps a missing container or volume to ErrNotFound.
func podmanNotFound(err error) error {
	if isNoSuch(err) {
		return ErrNotFound
	}
	return err
}

// agentUID is fixed; the Containerfile's agent stage says why.
const agentUID = 1000

// pidsLimit stops a fork bomb, with room for builds that start many processes
// and threads.
const pidsLimit = 4096
