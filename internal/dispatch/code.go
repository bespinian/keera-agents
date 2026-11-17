package dispatch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// Agents work in repositories with git, through the control plane's git
// proxy, and with these tools for issues and pull or merge requests. Both
// use the connection's token, or the token of the person the task is for,
// which never reaches the container, and both reach only the repositories
// an agent is bound to.

// cloneCommand clones a repository in an agent's container. It clones from
// the runtime's local proxy, which passes it on to the control plane.
func cloneCommand(b store.Binding) string {
	return fmt.Sprintf("git clone %s/%s/%s.git", protocol.GitProxyURL, b.ID, path.Base(b.Target))
}

func codeTools(bindings []store.Binding) []protocol.ToolSpec {
	list := bindingList(bindings, "Your repositories", func(b store.Binding) bool { return b.Mode == store.ModeRepo })
	if list == "" {
		return nil
	}
	kind := `"kind":{"type":"string","enum":["issue","change"],"description":"issue, or change for a pull or merge request. Defaults to issue."}`
	return []protocol.ToolSpec{
		{
			Name: "repo_list_issues",
			Description: "List issues, or pull or merge requests, in one of your repositories, newest change first. " +
				"Clone a repository with git to read and change its code. You may push only branches whose name " +
				"starts with " + protocol.BranchPrefix + "." + list,
			Parameters: schema(`{"type":"object","properties":{
				"repository":{"type":"string","description":"A repository id from the list."},` + kind + `,
				"state":{"type":"string","enum":["open","closed","all"]}},
				"required":["repository"]}`),
		},
		{
			Name:        "repo_read_issue",
			Description: "Read an issue, or a pull or merge request, with its comments.",
			Parameters: schema(`{"type":"object","properties":{
				"repository":{"type":"string"},"number":{"type":"integer"},` + kind + `},
				"required":["repository","number"]}`),
		},
		{
			Name:        "repo_comment",
			Description: "Comment on an issue, or a pull or merge request, in Markdown.",
			Parameters: schema(`{"type":"object","properties":{
				"repository":{"type":"string"},"number":{"type":"integer"},` + kind + `,
				"body":{"type":"string"}},
				"required":["repository","number","body"]}`),
		},
		{
			Name: "repo_open_change",
			Description: "Open a pull request (GitHub) or merge request (GitLab) for a branch you pushed. " +
				"Say what you changed and why, and name the issue it fixes.",
			Parameters: schema(`{"type":"object","properties":{
				"repository":{"type":"string"},
				"branch":{"type":"string","description":"The branch you pushed, starting with ` + protocol.BranchPrefix + `."},
				"base":{"type":"string","description":"The branch to merge into. Defaults to the repository's default branch."},
				"title":{"type":"string"},"body":{"type":"string"},
				"draft":{"type":"boolean","description":"Open it as a draft, for work that is not finished."}},
				"required":["repository","branch","title"]}`),
		},
	}
}

// LookupRepo checks a connection reaches a repository and may push to it,
// as userID, who is binding it, when people sign in to it.
func (d *Dispatcher) LookupRepo(ctx context.Context, c store.Connection, target, userID string) (connectors.Repo, error) {
	adapter, ok := connectors.CodeFor(connectors.Kind(c.Kind))
	if !ok {
		return connectors.Repo{}, fmt.Errorf("no code host for %s", c.Kind)
	}
	conn, err := d.ConnFor(ctx, c, userID)
	if err != nil {
		return connectors.Repo{}, err
	}
	return adapter.Repo(ctx, conn, target)
}

// repo returns the agent's repository binding, its adapter and its
// connection, for task t.
func (d *Dispatcher) repo(ctx context.Context, a store.Agent, t store.Task, bindingID string) (store.Binding, connectors.Code, connectors.Connection, error) {
	return bound(ctx, d, a, t, bindingID, "repositories", connectors.CodeFor, store.ModeRepo)
}

// GitRemote is where the git proxy sends an agent's request for one of its
// repositories, during task t, with the credentials to add.
func (d *Dispatcher) GitRemote(ctx context.Context, a store.Agent, t store.Task, bindingID string) (connectors.Remote, error) {
	b, adapter, conn, err := d.repo(ctx, a, t, bindingID)
	if err != nil {
		return connectors.Remote{}, err
	}
	return adapter.Remote(ctx, conn, b.Target)
}

func isChange(kind string) (bool, error) {
	switch kind {
	case "", "issue":
		return false, nil
	case "change":
		return true, nil
	}
	return false, fmt.Errorf("kind must be issue or change, not %q", kind)
}

// issueState checks a state for Issues, which takes open, closed or all.
func issueState(state string) (string, error) {
	switch state {
	case "":
		return "open", nil
	case "open", "closed", "all":
		return state, nil
	}
	return "", fmt.Errorf("state must be open, closed or all, not %q", state)
}

func (d *Dispatcher) toolRepoList(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	change, err := isChange(in.Kind)
	if err != nil {
		return nil, err
	}
	state, err := issueState(in.State)
	if err != nil {
		return nil, err
	}
	b, adapter, conn, err := d.repo(ctx, a, t, in.Repo)
	if err != nil {
		return nil, err
	}
	return adapter.Issues(ctx, conn, b.Target, state, change)
}

func (d *Dispatcher) toolRepoRead(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	change, err := isChange(in.Kind)
	if err != nil {
		return nil, err
	}
	if in.Number < 1 {
		return nil, errors.New("give the number of the issue or change")
	}
	b, adapter, conn, err := d.repo(ctx, a, t, in.Repo)
	if err != nil {
		return nil, err
	}
	return adapter.Issue(ctx, conn, b.Target, in.Number, change)
}

func (d *Dispatcher) toolRepoComment(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	change, err := isChange(in.Kind)
	if err != nil {
		return nil, err
	}
	body := value(in.Body)
	if in.Number < 1 || body == "" {
		return nil, errors.New("give the number of the issue or change, and the comment")
	}
	b, adapter, conn, err := d.repo(ctx, a, t, in.Repo)
	if err != nil {
		return nil, err
	}
	if err := adapter.Comment(ctx, conn, b.Target, in.Number, change, body); err != nil {
		return nil, err
	}
	return "commented", nil
}

func (d *Dispatcher) toolRepoOpenChange(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	c := connectors.Change{
		Branch: in.Branch, Base: in.Base, Title: value(in.Title), Body: value(in.Body), Draft: in.Draft,
	}
	if !strings.HasPrefix(c.Branch, protocol.BranchPrefix) {
		return nil, fmt.Errorf("you may only push branches starting with %s, so the change must come from one", protocol.BranchPrefix)
	}
	if c.Title == "" {
		return nil, errors.New("give the change a title")
	}
	b, adapter, conn, err := d.repo(ctx, a, t, in.Repo)
	if err != nil {
		return nil, err
	}
	if c.Base == "" {
		repo, err := adapter.Repo(ctx, conn, b.Target)
		if err != nil {
			return nil, err
		}
		c.Base = repo.DefaultBranch
	}
	return adapter.OpenChange(ctx, conn, b.Target, c)
}
