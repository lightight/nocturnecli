package app

import (
	"context"
	"fmt"
	"strings"
)

// maxSubagentRounds caps a sub-agent's tool loop so a stuck task can't run forever.
const maxSubagentRounds = 15

type subagentProgress struct {
	Percent int
	Latest  string
}

// runSubagent runs the task tool: a nested agent loop mirroring runHeadless —
// a fresh conversation seeded with the task prompt, non-streaming requests,
// tools auto-accepted (the user already consented at the task approval prompt).
// It returns the sub-agent's final narration / finish summary as the report
// that becomes the task tool's result.
//
// The sub-agent is forced to extended thinking and gets a system prompt with no
// cowork, no plan mode, and no task tool, so sub-agents can't spawn sub-agents.
func runSubagent(ctx context.Context, cfg *Config, client *Client, workdir, prompt string) (string, error) {
	return runSubagentWithLimit(ctx, cfg, client, workdir, prompt, maxSubagentRounds)
}

func runSubagentWithLimit(ctx context.Context, cfg *Config, client *Client, workdir, prompt string, maxRounds int) (string, error) {
	return runSubagentWithProgress(ctx, cfg, client, workdir, prompt, maxRounds, nil)
}

func runSubagentWithProgress(ctx context.Context, cfg *Config, client *Client, workdir, prompt string, maxRounds int, progress func(subagentProgress)) (string, error) {
	if maxRounds <= 0 {
		maxRounds = maxSubagentRounds
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("sub-agent: task requires a 'prompt'")
	}
	reportProgress := func(percent int, latest string) {
		if progress == nil {
			return
		}
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		progress(subagentProgress{Percent: percent, Latest: latest})
	}

	// Force extended thinking for the sub-agent without disturbing the caller's
	// config. Config and Client hold only value fields, pointers, and maps (no
	// mutexes), so shallow copies are safe here.
	subCfg := *cfg
	subCfg.Level = "extended"
	sub := *client
	sub.cfg = &subCfg

	system := systemPromptModeWithTools(workdir, false, false, false, false, subCfg.Tools)
	msgs := []ChatMessage{{Role: "user", Content: prompt}}

	var lastNarration string
	emptyNudges := 0
	var badCalls badCallBreaker
	for round := 0; round < maxRounds; round++ {
		pct := 5 + int(float64(round)/float64(maxRounds)*85)
		reportProgress(pct, fmt.Sprintf("round %d/%d: asking model", round+1, maxRounds))
		res, err := sub.Chat(ctx, system, msgs)
		if err != nil {
			reportProgress(100, "failed: "+oneLine(err.Error(), 90))
			return "", fmt.Errorf("sub-agent: %w", err)
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: res.Text})

		narration, calls := parseResponse(res.Text)
		if len(calls) == 0 {
			report := strings.TrimSpace(narration)
			if report == "" || intentOnlyReply(report) {
				if emptyNudges < maxEmptyNudges {
					emptyNudges++
					reportProgress(pct, "reply had no tool call — nudging model")
					msgs = append(msgs, ChatMessage{Role: "user", Content: emptyReplyNudge})
					continue
				}
				reportProgress(100, "failed: finished without report")
				return "", fmt.Errorf("sub-agent: finished without a report")
			}
			reportProgress(100, "finished")
			return report, nil
		}
		emptyNudges = 0
		if narration != "" {
			lastNarration = narration
			reportProgress(pct, oneLine(narration, 90))
		}

		var results []toolResult
		var images []Image
		for _, tc := range calls {
			reportProgress(pct, "running "+tc.summarize())
			if out, bad := diagnoseBadToolCall(tc, subCfg.Tools); bad {
				if badCalls.add(tc) {
					reportProgress(100, "failed: repeated bad tool call")
					return "", fmt.Errorf("sub-agent: the model emitted the same malformed tool call (%s) %d times in a row — giving up", tc.Name, maxBadCallRepeats)
				}
				results = append(results, toolResult{Name: tc.Name, Output: out})
				continue
			}
			badCalls.reset()
			if canonicalTool(tc.Name) == "finish" {
				report := strings.TrimSpace(argStr(tc.Args, "summary"))
				if report == "" {
					report = strings.TrimSpace(narration)
				}
				if report == "" {
					report = lastNarration
				}
				if report == "" {
					report = "Sub-agent finished (no summary given)."
				}
				reportProgress(100, "finished")
				return report, nil
			}
			if canonicalTool(tc.Name) == "install_skill" {
				results = append(results, toolResult{Name: tc.Name, Output: installSkillTool(&subCfg, tc.Args)})
				continue
			}
			out, img := executeWithImageWithTools(tc, workdir, sub.supportsVision(subCfg.Model), func(im Image) (string, error) {
				return sub.DescribeScreenshot(ctx, VisionModel, im)
			}, subCfg.Tools)
			if img != nil {
				images = append(images, *img)
			}
			results = append(results, toolResult{Name: tc.Name, Output: out, Image: img})
			reportProgress(pct, "completed "+tc.summarize())
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: buildToolResults(results), Images: images})
	}
	reportProgress(100, fmt.Sprintf("failed: reached max %d rounds", maxRounds))
	return "", fmt.Errorf("sub-agent: reached max %d rounds without finishing", maxRounds)
}
