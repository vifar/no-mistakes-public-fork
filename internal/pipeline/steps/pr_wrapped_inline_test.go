package steps

import (
	"html"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRWrappedInlineClaimsPublication(t *testing.T) {
	for _, wrapper := range [][2]string{{"**", "**"}, {"*", "*"}, {"__", "__"}, {"_", "_"}, {"~~", "~~"}, {"***", "***"}, {"**_", "_**"}} {
		for _, tc := range []struct{ name, raw, want string }{
			{"AI", "Co-Authored-By: Codex <bot@example.invalid>", prAIAttributionNotice},
			{"human", "Co-Authored-By: Ada <ada@example.com>", "co-author (quoted): Ada <ada@example.com>"},
			{"field", `The field "Co-Authored-By:" identifies Ada <ada@example.com>`, `The field "Co-Authored-By:" identifies Ada <ada@example.com>`},
			{"code", "`Co-Authored-By: Ada <ada@example.com>`", "`Co-Authored-By: Ada <ada@example.com>`"},
		} {
			t.Run(wrapper[0]+"/"+tc.name, func(t *testing.T) {
				raw := "Recorded risk " + wrapper[0] + tc.raw + wrapper[1] + "."
				want := "Recorded risk " + wrapper[0] + tc.want + wrapper[1] + "."
				for _, recorded := range []bool{false, true} {
					input, expected := raw, want
					if recorded {
						const prefix = `{ "summary" : `
						const suffix = `, "escape": "A\u0042\/C", "number": 1.200e+3 }`
						literal := strings.NewReplacer("<", `\u003c`, ">", `\u003e`).Replace(strconv.Quote(raw))
						input = prefix + literal + suffix
						if raw == want {
							expected = input
						} else {
							expected = prefix + strconv.Quote(want) + suffix
						}
					}
					for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
						for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
							ctx := &pipeline.StepContext{Config: &config.Config{}}
							ctx.Config.PR.Appendix = mode
							for path, body := range map[string]string{
								"assemble": assemblePRBody(ctx, "## What Changed\n\nPreserve repair summaries.", input, "", "", 0, provider),
								"build":    buildPRBodyWithin("## What Changed\n\nPreserve repair summaries.", input, "", "", ctx, provider, maxPullRequestBodyBytes),
								"evidence": appendixEvidence(mode, prBodyFlavorFor(provider), input, "", "", provider),
							} {
								view := html.UnescapeString(body)
								pathExpected := expected
								if path != "evidence" {
									pathExpected = strings.ReplaceAll(pathExpected, prAIAttributionNotice, "")
								}
								if !strings.Contains(view, pathExpected) {
									t.Errorf("%s/%s/%s recorded=%t: missing %q in %q", provider, mode, path, recorded, pathExpected, view)
								}
								if tc.name == "AI" && (strings.Contains(view, "Co-Authored-By:") || strings.Contains(view, "Codex") || strings.Contains(view, "bot@example.invalid")) {
									t.Errorf("%s/%s/%s recorded=%t: published AI attribution: %q", provider, mode, path, recorded, view)
								}
							}
						}
					}
					bodyExpected := strings.ReplaceAll(expected, prAIAttributionNotice, "")
					content, err := composeOwnedPRContent(prOwnedBody{before: input + "\n"}, "fix: preserve repair summaries", compliantPipelineBody(t, testPipelineHeadSHA), 0)
					if err != nil || !strings.Contains(content.Body, bodyExpected) {
						t.Errorf("owned recorded=%t: missing %q in %q, error=%v", recorded, bodyExpected, content.Body, err)
					}
				}
			})
		}
	}
}
