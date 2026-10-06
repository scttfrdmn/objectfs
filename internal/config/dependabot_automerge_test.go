package config

import (
	"strings"
	"testing"
)

// mergeIdentityExpr is the expression the auto-merge step must take its token from. Named for what
// it decides rather than what it holds: a name containing "token" trips gosec G101, which reads any
// string constant so named as a hardcoded credential, and this one is a reference to a secret.
//
// Not secrets.GITHUB_TOKEN, and the difference has no symptom anywhere a person looks. GitHub creates
// no workflow run for an event made with the workflow's own token, and when `gh pr merge --auto`
// fires, the merge is attributed to the token that armed it. So with GITHUB_TOKEN every Dependabot
// merge reached `main` with no ci.yml and no Security Scan run on it — #555, #564, #577 and #582 —
// while `main` still read green, because its status is its latest run and that was the last merge a
// person made. #584.
const mergeIdentityExpr = "${{ secrets.AUTOMERGE_TOKEN }}"

// TestDependabotAutoMergeUsesATokenWhosePushesRunCI asserts the step that arms auto-merge does so with
// a token whose merges trigger workflows.
//
// It also refuses a fallback such as `secrets.AUTOMERGE_TOKEN || secrets.GITHUB_TOKEN`. That keeps
// auto-merge working when the secret is missing by quietly reopening the gap, and "it still merges"
// is exactly what made the gap invisible the first time.
func TestDependabotAutoMergeUsesATokenWhosePushesRunCI(t *testing.T) {
	t.Parallel()

	wf := readWorkflow(t, "dependabot-automerge.yml")

	merging := 0

	for _, id := range sortedMapKeys(wf.Jobs) {
		for _, step := range jobSteps(wf.Jobs[id]) {
			if !strings.Contains(withoutComments(step.Run), "gh pr merge") {
				continue
			}

			merging++

			// gh reads GH_TOKEN ahead of GITHUB_TOKEN, so a GH_TOKEN set to anything else would be
			// the token actually used, whatever GITHUB_TOKEN says.
			for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
				value, set := step.Env[name]
				if !set {
					continue
				}

				if value != mergeIdentityExpr {
					t.Errorf("dependabot-automerge.yml job %q step %q runs `gh pr merge` with %s=%s, "+
						"want %s.\n"+
						"A merge armed with the workflow's own GITHUB_TOKEN reaches main with no CI run on "+
						"it, because GitHub creates no workflow run for an event made with that token, and "+
						"main still reads green because its status is the last run a person caused. A "+
						"fallback to GITHUB_TOKEN reopens the same gap whenever the secret is missing. #584.",
						id, step.Name, name, value, mergeIdentityExpr)
				}
			}

			if step.Env["GH_TOKEN"] == "" && step.Env["GITHUB_TOKEN"] == "" {
				t.Errorf("dependabot-automerge.yml job %q step %q runs `gh pr merge` with no token in "+
					"its env, so gh falls back to whatever the runner has, which is nothing. Set "+
					"GITHUB_TOKEN: %s", id, step.Name, mergeIdentityExpr)
			}
		}
	}

	// Exactly one. Zero means the step moved or was renamed out of `run:` and every assertion above
	// passed having inspected nothing. Two means a second merge path exists that this test was not
	// written knowing about. Both need a person to look.
	if merging != 1 {
		t.Fatalf("found %d steps in dependabot-automerge.yml that run `gh pr merge`, want exactly 1", merging)
	}
}
