package app

import (
	"context"
	"fmt"
	"strings"
)

// maxSubagentRounds caps a sub-agent's tool loop so a stuck task can't run forever.
const maxSubagentRounds = 15

// runSubagent runs the task tool: a nested agent loop mirroring runHeadless —
// a fresh conversation seeded with the task prompt, non-streaming requests,
// tools auto-accepted (the user already consented at the task approval prompt).
// It returns the sub-agent's final narration / finish summary as the report
// that becomes the task tool's result.
//
// The sub-agent is forced to extended thinking and gets a system prompt with no
// cowork, no plan mode, and no task tool, so sub-agents can't spawn sub-agents.
func runSubagent(ctx context.Context, cfg *Config, client *Client, workdir, prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("sub-agent: task requires a 'prompt'")
	}

	// Force extended thinking for the sub-agent without disturbing the caller's
	// config. Config and Client hold only value fields, pointers, and maps (no
	// mutexes), so shallow copies are safe here.
	subCfg := *cfg
	subCfg.Level = "extended"
	sub := *client
	sub.cfg = &subCfg

	system := systemPromptMode(workdir, false, false, false)
	msgs := []ChatMessage{{Role: "user", Content: prompt}}

	var lastNarration string
	for round := 0; round < maxSubagentRounds; round++ {
		res, err := sub.Chat(ctx, system, msgs)
		if err != nil {
			return "", fmt.Errorf("sub-agent: %w", err)
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: res.Text})

		narration, calls := parseResponse(res.Text)
		if len(calls) == 0 {
			report := strings.TrimSpace(narration)
			if report == "" {
				return "", fmt.Errorf("sub-agent: finished without a report")
			}
			return report, nil
		}
		if narration != "" {
			lastNarration = narration
		}

		var results []toolResult
		var images []Image
		for _, tc := range calls {
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
				return report, nil
			}
			out, img := executeWithImage(tc, workdir, sub.supportsVision(subCfg.Model), func(im Image) (string, error) {
				return sub.DescribeScreenshot(ctx, VisionModel, im)
			})
			if img != nil {
				images = append(images, *img)
			}
			results = append(results, toolResult{Name: tc.Name, Output: out, Image: img})
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: buildToolResults(results), Images: images})
	}
	return "", fmt.Errorf("sub-agent: reached max %d rounds without finishing", maxSubagentRounds)
}
