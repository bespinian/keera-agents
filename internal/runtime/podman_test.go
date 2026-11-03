package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPodmanKeepsSecretsOffItsCommandLine(t *testing.T) {
	// Every user can read a process's arguments.
	t.Setenv("PATH", "/usr/bin")
	p := &Podman{}
	spec := testSpec()
	spec.Env["PATH"] = "/opt/bin:/usr/bin"
	args, env := p.runArgs(spec, "")
	line := strings.Join(args, " ")
	if strings.Contains(line, "rt_secret") {
		t.Errorf("the command line holds the runtime token: %s", line)
	}
	if !slices.Contains(args, "KEERA_AGENTS_RUNTIME_TOKEN") ||
		!slices.Contains(env, "KEERA_AGENTS_RUNTIME_TOKEN=rt_secret") {
		t.Errorf("the token is not handed to podman through its environment: %v / %v", args, env)
	}
	// podman would otherwise run with the agent's PATH.
	if !slices.Contains(args, "PATH=/opt/bin:/usr/bin") || slices.Contains(env, "PATH=/opt/bin:/usr/bin") {
		t.Errorf("a name podman has should stay on the command line: %v / %v", args, env)
	}
}

// standardClass is a complete class, as drivers get one.
var standardClass = Class{Isolation: Standard}.WithDefaults()

func TestPodmanRunArgs(t *testing.T) {
	p := &Podman{opts: PodmanOptions{Network: "keera"}}
	args, _ := p.runArgs(testSpec(), "krun")
	line := strings.Join(args, " ")
	for _, want := range []string{
		"--name keera-agent-agt-06c1k2rt8g3m4n5p6q7r8s9t0v",
		"--runtime krun",
		"--network keera",
		"--cpus 2.000",
		"--memory 4294967296 --memory-swap 4294967296",
		"--security-opt no-new-privileges",
		"--cap-drop ALL",
		"--read-only --read-only-tmpfs=false --tmpfs /tmp:",
		"--pids-limit 4096",
		"--user 1000:1000",
		"--volume keera-agent-agt-06c1k2rt8g3m4n5p6q7r8s9t0v-workspace:/workspace",
		"--label keera.dev/owner=ada@example.ch",
		"--label keera.dev/org=org_1",
		"--label keera.dev/project=prj_1",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the command line lacks %q: %s", want, line)
		}
	}
	if args[len(args)-1] != "registry.example/keera-agent:1" {
		t.Errorf("the image must come last: %v", args)
	}
}

// scriptedPodman runs script, logs every call to the returned file, and
// fails whatever the script lets through.
func scriptedPodman(t *testing.T, script string) (*Podman, string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "podman"), filepath.Join(dir, "calls")
	body := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + script +
		"echo \"unexpected: $*\" >&2; exit 125\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Podman{opts: PodmanOptions{Binary: bin}}, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func callsStarting(all []string, prefix string) []string {
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func inspectReturns(state, labels string) string {
	return "if [ \"$1\" = inspect ]; then echo '[{\"State\":" + state +
		",\"Config\":{\"Labels\":" + labels + "}}]'; exit 0; fi\n"
}

const noSuchContainer = "if [ \"$1\" = inspect ]; then echo 'Error: no such container' >&2; exit 125; fi\n"

func TestPodmanCreateRunsANewContainer(t *testing.T) {
	p, log := scriptedPodman(t, noSuchContainer+
		"if [ \"$1\" = run ]; then exit 0; fi\n")
	st, err := p.Create(context.Background(), Spec{AgentID: "agt_1", Image: "img", Class: standardClass})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The fake still answers that there is no container.
	if st.State != Gone {
		t.Errorf("state = %s", st.State)
	}
	if runs := callsStarting(calls(t, log), "run "); len(runs) != 1 {
		t.Errorf("runs = %v, want one", runs)
	}
}

// Each start brings a new token, so a container that is there is replaced.
func TestPodmanCreateReplacesTheContainerAndKeepsTheVolume(t *testing.T) {
	p, log := scriptedPodman(t,
		inspectReturns(`{"Status":"running","Running":true}`, "{}")+
			"if [ \"$1\" = rm ] || [ \"$1\" = run ]; then exit 0; fi\n")
	if _, err := p.Create(context.Background(), Spec{AgentID: "agt_1", Image: "img:2", Class: standardClass}); err != nil {
		t.Fatalf("create: %v", err)
	}
	all := calls(t, log)
	rms, runs := callsStarting(all, "rm "), callsStarting(all, "run ")
	if len(rms) != 1 || len(runs) != 1 {
		t.Fatalf("calls = %v, want one rm and one run", all)
	}
	if strings.Contains(rms[0], "--volumes") || len(callsStarting(all, "volume ")) != 0 {
		t.Errorf("the volume must be kept: %v", all)
	}
	if !strings.Contains(runs[0], "--volume keera-agent-agt-1-workspace:/workspace") {
		t.Errorf("the new container does not mount the old volume: %s", runs[0])
	}
}

func TestPodmanCreateRefusesAnUnmappedTier(t *testing.T) {
	p, log := scriptedPodman(t, "")
	_, err := p.Create(context.Background(), Spec{AgentID: "agt_1", Image: "img", Class: Class{Isolation: Isolated}.WithDefaults()})
	if !errors.Is(err, ErrTier) {
		t.Fatalf("Create = %v, want ErrTier", err)
	}
	if c := calls(t, log); len(c) != 0 {
		t.Errorf("podman was run: %v", c)
	}
}

func TestPodmanStatus(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  State
	}{
		{`{"Status":"running","Running":true}`, Running},
		{`{"Status":"created"}`, Pending},
		{`{"Status":"exited","ExitCode":0}`, Suspended},
		{`{"Status":"exited","ExitCode":143}`, Suspended},
		{`{"Status":"exited","ExitCode":137,"OOMKilled":true}`, Failed},
		{`{"Status":"exited","ExitCode":2}`, Failed},
	} {
		p, _ := scriptedPodman(t, inspectReturns(tc.state, "{}"))
		st, err := p.Status(context.Background(), "agt_1")
		if err != nil || st.State != tc.want {
			t.Errorf("%s: Status = %+v, %v; want %s", tc.state, st, err, tc.want)
		}
	}
	p, _ := scriptedPodman(t, noSuchContainer)
	if st, err := p.Status(context.Background(), "agt_1"); err != nil || st.State != Gone {
		t.Errorf("missing container: Status = %+v, %v; want gone", st, err)
	}
}

// The fleet compares a running container's token with the stored one.
func TestPodmanStatusHasTheToken(t *testing.T) {
	p, _ := scriptedPodman(t, inspectReturns(`{"Status":"running","Running":true}`,
		`{},"Env":["PATH=/bin","KEERA_AGENTS_RUNTIME_TOKEN=kat_1"]`))
	st, err := p.Status(context.Background(), "agt_1")
	if err != nil || st.State != Running || st.Token != "kat_1" {
		t.Errorf("Status = %+v, %v; want running with kat_1", st, err)
	}
}

func TestPodmanTerminateRemovesTheVolumeAndIsIdempotent(t *testing.T) {
	p, log := scriptedPodman(t,
		"if [ \"$1\" = rm ]; then echo 'Error: no such container' >&2; exit 1; fi\n"+
			"if [ \"$1\" = volume ]; then echo 'Error: no such volume' >&2; exit 1; fi\n")
	if err := p.Terminate(context.Background(), "agt_1"); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if v := callsStarting(calls(t, log), "volume rm --force keera-agent-agt-1-workspace"); len(v) != 1 {
		t.Errorf("the volume was not removed: %v", calls(t, log))
	}
}

func TestPodmanSuspendAMissingAgent(t *testing.T) {
	p, _ := scriptedPodman(t, "if [ \"$1\" = stop ]; then echo 'Error: no such container' >&2; exit 125; fi\n")
	if err := p.Suspend(context.Background(), "agt_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Suspend = %v, want ErrNotFound", err)
	}
}
