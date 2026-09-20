package execenv

import (
	"strings"
	"testing"
)

// A project pins a repo to a branch because its work lives on that line
// (MUL-7504). The daemon already checks the repo out there, so the two things
// the BRIEF has to add are the ones the agent cannot infer from a working
// directory: which line it is on, and that delivery goes back to the same one.
//
// The second matters most. Nothing in the platform creates pull requests —
// `gh pr create` is the agent's own call, and without --base it targets the
// repository's default branch. A PR opened that way asks to merge into the
// wrong place AND carries every commit the pinned branch has that the default
// lacks.
func TestRepositoriesSectionCarriesPinnedRefAndDeliveryTarget(t *testing.T) {
	t.Parallel()
	ctx := TaskContextForEnv{
		IssueID: "i-1", AgentName: "Eve", AgentID: "eve-1",
		Repos: []RepoContextForEnv{
			{URL: "https://github.com/multica-ai/multica", Ref: "release/2026-09"},
		},
	}
	out := buildMetaSkillContent("claude", ctx)

	if !strings.Contains(out, "https://github.com/multica-ai/multica (starts from `release/2026-09`)") {
		t.Errorf("Repositories section should name the pinned ref beside its repo; got:\n%s", out)
	}
	if !strings.Contains(out, "gh pr create --base") {
		t.Errorf("brief should tell the agent to target the same branch when delivering; got:\n%s", out)
	}
	// A pin is already applied on checkout. An agent that re-passes --ref to
	// "get back to" the branch is doing redundant work at best, and pinning a
	// stale value at worst.
	if !strings.Contains(out, "do not pass `--ref` to get back to it") {
		t.Errorf("brief should say the checkout is already at the pinned ref; got:\n%s", out)
	}
}

// The delivery rule is stated only when something is actually pinned. An
// unpinned workspace repo already starts from its default branch, and
// `gh pr create` already targets that — an unconditional --base instruction
// would be noise in the common case.
func TestRepositoriesSectionOmitsDeliveryRuleWhenNothingIsPinned(t *testing.T) {
	t.Parallel()
	ctx := TaskContextForEnv{
		IssueID: "i-1", AgentName: "Eve", AgentID: "eve-1",
		Repos: []RepoContextForEnv{
			{URL: "https://github.com/multica-ai/multica", Description: "the platform"},
		},
	}
	out := buildMetaSkillContent("claude", ctx)

	if !strings.Contains(out, "https://github.com/multica-ai/multica — the platform") {
		t.Errorf("unpinned repo should still render its description; got:\n%s", out)
	}
	if strings.Contains(out, "starts from") || strings.Contains(out, "gh pr create --base") {
		t.Errorf("no repo is pinned, so the brief must not mention a starting point or --base; got:\n%s", out)
	}
}

// A pin is not necessarily a branch — the field accepts anything git resolves,
// and neither the server nor the daemon can tell a branch from a tag or a
// commit without asking the remote, which the product deliberately does not do.
// A tag has no branch to merge back into, so the delivery rule has to be stated
// conditionally rather than as "always --base whatever is pinned".
func TestDeliveryRuleDoesNotAssumeThePinIsABranch(t *testing.T) {
	t.Parallel()
	ctx := TaskContextForEnv{
		IssueID: "i-1", AgentName: "Eve", AgentID: "eve-1",
		Repos: []RepoContextForEnv{{URL: "https://github.com/o/r", Ref: "v1.4.0"}},
	}
	out := buildMetaSkillContent("claude", ctx)

	if !strings.Contains(out, "tag or a commit rather than a branch") {
		t.Errorf("brief should carve out the tag/commit case; got:\n%s", out)
	}
	if !strings.Contains(out, "confirm the target branch") {
		t.Errorf("brief should tell the agent to confirm rather than guess a base; got:\n%s", out)
	}
}

// The Project Context bullet used to read as though --ref were how you reach
// the configured revision. It is not: the daemon applies it, and --ref is the
// override. An agent following the old wording would re-specify a value it
// already had — and would keep using a stale one after the project changed.
func TestProjectContextSaysThePinIsAlreadyApplied(t *testing.T) {
	t.Parallel()
	ctx := TaskContextForEnv{
		IssueID: "i-1", AgentName: "Eve", AgentID: "eve-1",
		ProjectID: "p-1", ProjectTitle: "Release line",
		ProjectResources: []ProjectResourceForEnv{{
			ResourceType: "github_repo",
			ResourceRef:  []byte(`{"url":"https://github.com/o/r","ref":"release/2026-09"}`),
		}},
	}
	out := buildMetaSkillContent("claude", ctx)

	if !strings.Contains(out, "checked out there automatically") {
		t.Errorf("Project Context should say the configured start is applied for you; got:\n%s", out)
	}
	if !strings.Contains(out, "only to override it") {
		t.Errorf("Project Context should frame --ref as the override; got:\n%s", out)
	}
}
