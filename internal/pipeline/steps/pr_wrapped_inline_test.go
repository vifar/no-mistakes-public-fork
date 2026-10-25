package steps

import (
	"encoding/json"
	"html"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRWrappedInlineClaimsPublication(t *testing.T) {
	for _, wrapper := range [][3]string{
		{"**", "**", "."}, {"*", "*", "."}, {"__", "__", "."}, {"_", "_", "."},
		{"~~", "~~", "."}, {"***", "***", "."}, {"**_", "_**", "."},
		{"**", ".**", ""}, {"*", ".*", "!"}, {"__", ";__", "?"}, {"_", ":_", "."},
		{"~~", "!~~", "."}, {"***", "?***", "."}, {"**_", "._**", ""}, {"**_", "._!**", "?"},
		{"***", "**.*", ""}, {"***", "*.**", "!"}, {"___", "__._", ""}, {"___", "_;__", "?"},
		{"**_", "_*.*", "."},
	} {
		for _, tc := range []struct{ name, raw, want string }{
			{"AI", "Co-Authored-By: Codex <bot@example.invalid>", prAIAttributionNotice},
			{"configured", "Co-Authored-By: Example, Inc. Bot <opaque@example.com>", prAIAttributionNotice},
			{"human", "Co-Authored-By: Ada <ada@example.com>", "co-author (quoted): Ada <ada@example.com>"},
			{"field", `The field "Co-Authored-By:" identifies Ada <ada@example.com>`, `The field "Co-Authored-By:" identifies Ada <ada@example.com>`},
			{"code", "`Co-Authored-By: Ada <ada@example.com>`", "`Co-Authored-By: Ada <ada@example.com>`"},
			{"link", "[Co-Authored-By: Codex <ai@example.com>](https://example.com)", "[Co-Authored-By: Codex <ai@example.com>](https://example.com)"},
		} {
			t.Run(wrapper[0]+"/"+tc.name+"/"+wrapper[1]+wrapper[2], func(t *testing.T) {
				raw := "Recorded risk " + wrapper[0] + tc.raw + wrapper[1] + wrapper[2]
				want := "Recorded risk " + wrapper[0] + tc.want + wrapper[1] + wrapper[2]
				var trailers []string
				if tc.name == "configured" {
					trailers = []string{tc.raw}
				}
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
							evidenceInput := input
							if len(trailers) > 0 {
								evidenceInput = prDisplayNoticeText(input, trailers...)
							}
							for path, body := range map[string]string{
								"assemble": assemblePRBody(ctx, "## What Changed\n\nPreserve repair summaries.", input, "", "", 0, provider, trailers...),
								"build":    buildPRBodyWithin("## What Changed\n\nPreserve repair summaries.", input, "", "", ctx, provider, maxPullRequestBodyBytes, trailers...),
								"evidence": appendixEvidence(mode, prBodyFlavorFor(provider), evidenceInput, "", "", provider),
							} {
								view := html.UnescapeString(body)
								pathExpected := expected
								if path != "evidence" {
									pathExpected = strings.ReplaceAll(pathExpected, prAIAttributionNotice, "")
								}
								if !strings.Contains(view, pathExpected) {
									t.Errorf("%s/%s/%s recorded=%t: missing %q in %q", provider, mode, path, recorded, pathExpected, view)
								}
								if recorded {
									start, end := strings.Index(view, `{ "summary"`), strings.LastIndex(view, "}")
									if start < 0 || end < start || !json.Valid([]byte(view[start:end+1])) {
										t.Errorf("%s/%s/%s published invalid recorded JSON: %q", provider, mode, path, view)
									}
								}
								if (tc.name == "AI" || tc.name == "configured") && (strings.Contains(view, "Co-Authored-By:") || strings.Contains(view, "Codex") || strings.Contains(view, "bot@example.invalid") || strings.Contains(view, "opaque@example.com")) {
									t.Errorf("%s/%s/%s recorded=%t: published AI attribution: %q", provider, mode, path, recorded, view)
								}
							}
						}
					}
					bodyExpected := strings.ReplaceAll(expected, prAIAttributionNotice, "")
					content, err := composeOwnedPRContent(prOwnedBody{before: input + "\n"}, "fix: preserve repair summaries", compliantPipelineBody(t, testPipelineHeadSHA), 0, trailers...)
					if err != nil || !strings.Contains(content.Body, bodyExpected) {
						t.Errorf("owned recorded=%t: missing %q in %q, error=%v", recorded, bodyExpected, content.Body, err)
					}
				}
			})
		}
	}
}
