package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Version is the CLI version, overridable at build time with
// -ldflags "-X github.com/lightight/nocturnecli/internal/app.Version=...".
var Version = "0.3.0"

var cliHelp = `Nocturne — a terminal coding agent powered by the Nocturne API.

Usage:
  nocturne                  start the interactive TUI
  nocturne cowork           start the TUI in cowork (computer-use) mode
  nocturne -p "task"        run one task non-interactively (auto-runs tools)
  nocturne cowork -p "task" run one headless task with computer-use tools
  nocturne -m <model>       override the model for this run
  nocturne models           list the models your account can use
  nocturne update           update to the latest release
  nocturne update --check   check for a newer release without installing
  nocturne serve            host the docs, installers, and remote relay
  nocturne --version        print the version
  nocturne --help           show this help

Configuration:
  API key   NOCTURNE_API env var or a local .env; run /key in-app to save it
            permanently (private config, used from any directory).
            The key is verified on startup — if it has expired, create a new
            one at https://nocturne.lol/account
  Model     default ` + displayModel(DefaultModel) + `; pick with /model or -m
  Thinking  /level off · normal · extended (reasoning models; extended also
            enables the task tool — sub-agents with their own agent loop)
  Plan      /plan — the agent explores read-only and proposes a plan; run
            /plan again to approve and execute it
  Approvals /permissions — ask · auto-accept safe (AI-checked) · bypass
  Cowork    /cowork or "nocturne cowork" — the agent can see and control the
            screen (screenshot, click, type, scroll, open apps) and use the
            whole filesystem
  Config    ~/.config/nocturne/config.json (or the OS equivalent)
`

// Run is the CLI entry point.
func Run(args []string) error {
	// Subcommands.
	if len(args) > 0 && args[0] == "report-keygen" {
		// Team tool: mint the X25519 keypair for anonymous reports. The public
		// half is baked into builds (reportPublicKeyB64), the private half
		// stays with whoever decrypts reports.
		pub, priv, err := generateReportKey()
		if err != nil {
			return err
		}
		fmt.Println("public (bake into reportPublicKeyB64 or NOCTURNE_REPORT_PUBKEY):", pub)
		fmt.Println("private (keep secret, used by report-decrypt):", priv)
		return nil
	}
	if len(args) > 0 && args[0] == "report-decrypt" {
		// Team tool: open a sealed report envelope (file path or "-" for
		// stdin) with the private key from NOCTURNE_REPORT_PRIVKEY.
		priv := strings.TrimSpace(os.Getenv("NOCTURNE_REPORT_PRIVKEY"))
		if priv == "" {
			return fmt.Errorf("set NOCTURNE_REPORT_PRIVKEY to the private key from report-keygen")
		}
		if len(args) < 2 {
			return fmt.Errorf("usage: nocturne report-decrypt <envelope.json|->")
		}
		var data []byte
		var err error
		if args[1] == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(args[1])
		}
		if err != nil {
			return err
		}
		plain, err := decryptReport(data, priv)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, plain, "", "  ") == nil {
			fmt.Println(pretty.String())
		} else {
			fmt.Println(string(plain))
		}
		return nil
	}
	if len(args) > 0 && args[0] == "models" {
		cfg := LoadConfig()
		if err := checkStartupKey(cfg); err != nil {
			return err
		}
		models, def, err := NewClient(cfg).FetchModels(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("default: %s\n\n", displayModel(def))
		for _, md := range models {
			var tags []string
			if md.MaxTokens > 0 {
				tags = append(tags, "context "+formatTokenLimit(md.MaxTokens))
			}
			if md.Reasoning {
				tags = append(tags, "reasoning")
			}
			if md.Vision {
				tags = append(tags, "vision")
			}
			fmt.Printf("%-34s $%g/$%g  %s\n", displayModel(md.ID), md.InPrice, md.OutPrice, strings.Join(tags, " "))
		}
		return nil
	}
	if len(args) > 0 && args[0] == "update" {
		checkOnly := false
		for _, a := range args[1:] {
			if a == "--check" || a == "-c" {
				checkOnly = true
			}
		}
		msg, err := doUpdate(checkOnly)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	}

	var prompt, modelOverride string
	cowork := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "cowork":
			cowork = true
		case "--version", "-v":
			fmt.Println("nocturne " + Version)
			return nil
		case "--help", "-h":
			fmt.Print(cliHelp)
			return nil
		case "-p", "--print":
			if i+1 < len(args) {
				prompt = args[i+1]
				i++
			}
		case "-m", "--model":
			if i+1 < len(args) {
				modelOverride = args[i+1]
				i++
			}
		default:
			// A bare argument is treated as a one-shot prompt.
			if prompt == "" && args[i] != "" && args[i][0] != '-' {
				prompt = args[i]
			}
		}
	}

	cfg := LoadConfig()
	if modelOverride != "" {
		cfg.Model = normalizeModelID(modelOverride)
	}

	if prompt != "" {
		if err := checkStartupKey(cfg); err != nil {
			return err
		}
		return runHeadless(cfg, prompt, cowork)
	}
	return startTUI(cfg, Version, cowork)
}

// checkStartupKey verifies the configured API key with the server before any
// API-dependent command runs. A rejected key (expired/unknown) is a hard
// failure pointing the user at the account page; a server we can't reach is
// not — offline users aren't locked out.
func checkStartupKey(cfg *Config) error {
	if cfg.APIKey == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient(cfg).ValidateKey(ctx); errors.Is(err, ErrInvalidKey) {
		return fmt.Errorf("your Nocturne API key is invalid or has expired.\nCreate a new one at " + NewKeyURL)
	}
	return nil
}

// runHeadless executes a single task without the TUI: it runs the agent loop,
// auto-accepting tool calls, streaming activity to stderr and the final answer
// to stdout. Suited for scripting and piping.
func runHeadless(cfg *Config, prompt string, cowork bool) error {
	health := newHealthTracker()
	start := time.Now()
	err := runHeadlessLoop(cfg, prompt, cowork, health)
	headlessReportNote(cfg, health, start)
	return err
}

// headlessReportNote offers (never forces) an anonymous report after a bumpy
// run. NOCTURNE_SEND_REPORT=1 sends it; otherwise a one-line hint explains how.
func headlessReportNote(cfg *Config, health *healthTracker, start time.Time) {
	if health.issues() == 0 {
		return
	}
	if os.Getenv("NOCTURNE_SEND_REPORT") == "1" {
		payload := buildReport(health, cfg, Version, 0, start)
		if err := sendReport(cfg, payload); err != nil {
			fmt.Fprintln(os.Stderr, stErr.Render("  ✗ couldn't send report: "+err.Error()))
			return
		}
		fmt.Fprintln(os.Stderr, stOK.Render("  ✓ anonymous report "+payload.ID+" sent — sealed so only the team can open it"))
		return
	}
	fmt.Fprintf(os.Stderr, "  ◗ nocturne hit %d hiccups this run — re-run with NOCTURNE_SEND_REPORT=1 to send an anonymous, end-to-end-encrypted debug report (nothing is sent automatically)\n", health.issues())
}

func runHeadlessLoop(cfg *Config, prompt string, cowork bool, health *healthTracker) error {
	work, _ := os.Getwd()
	client := NewClient(cfg)
	msgs := []ChatMessage{{Role: "user", Content: prompt}}

	const maxRounds = 30
	emptyNudges := 0
	var badCalls badCallBreaker
	for round := 0; round < maxRounds; round++ {
		health.recordRound()
		res, err := client.Chat(context.Background(), systemPromptModeWithTools(work, cowork, false, false, cfg.Level == "extended", cfg.Tools), msgs)
		if err != nil {
			health.recordAPIError()
			return err
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: res.Text})

		narration, calls := parseResponse(res.Text)
		if len(calls) == 0 {
			if stalledReply(narration, calls) && emptyNudges < maxEmptyNudges {
				emptyNudges++
				health.recordEmptyReply()
				fmt.Fprintln(os.Stderr, stDim.Render("  ↻ reply had no tool call — asking the model to continue"))
				msgs = append(msgs, ChatMessage{Role: "user", Content: emptyReplyNudge})
				continue
			}
			fmt.Println(narration)
			return nil
		}
		emptyNudges = 0
		if narration != "" {
			fmt.Fprintln(os.Stderr, stDim.Render(narration))
		}

		var results []toolResult
		var images []Image
		for _, tc := range calls {
			if out, bad := diagnoseBadToolCall(tc, cfg.Tools); bad {
				fmt.Fprintln(os.Stderr, stAccent.Render("● ")+tc.summarize())
				fmt.Fprintln(os.Stderr, stErr.Render("  ✗ bad tool call — automatically asking model to retry"))
				health.recordBadCall(tc, out)
				if badCalls.add(tc) {
					health.recordBreakerTrip()
					return fmt.Errorf("the model emitted the same malformed tool call (%s) %d times in a row — stopping instead of burning more API calls; try rephrasing the task or a different model", tc.Name, maxBadCallRepeats)
				}
				results = append(results, toolResult{Name: tc.Name, Output: out})
				continue
			}
			badCalls.reset()
			if canonicalTool(tc.Name) == "finish" {
				summary := strings.TrimSpace(argStr(tc.Args, "summary"))
				if summary == "" {
					summary = narration
				}
				fmt.Println(summary)
				return nil
			}
			fmt.Fprintln(os.Stderr, stAccent.Render("● ")+tc.summarize())
			if canonicalTool(tc.Name) == "cowork" {
				// Headless auto-accepts: the model may widen its own powers here.
				cowork = true
				results = append(results, toolResult{Name: tc.Name,
					Output: "cowork mode enabled — you can now see and control the screen and use the whole filesystem."})
				continue
			}
			if canonicalTool(tc.Name) == "task" {
				if cfg.Level != "extended" {
					results = append(results, toolResult{Name: tc.Name,
						Output: "the task tool requires thinking level 'extended' — set it with /level extended"})
					continue
				}
				tasks := taskSpecsFromArgs(tc.Args)
				if len(tasks) == 0 {
					results = append(results, toolResult{Name: tc.Name, Output: "Error: task requires a prompt or non-empty tasks array"})
					continue
				}
				batchID := fmt.Sprintf("headless-subagents-%d", round)
				result := runSubagentBatch(batchID, tasks, false, false, cfg, client, work, nil)
				results = append(results, toolResult{Name: tc.Name, Output: result.Report})
				continue
			}
			if canonicalTool(tc.Name) == "install_skill" {
				results = append(results, toolResult{Name: tc.Name, Output: installSkillTool(cfg, tc.Args)})
				continue
			}
			out, img := executeWithImageWithTools(tc, work, client.supportsVision(cfg.Model), func(im Image) (string, error) {
				return client.DescribeScreenshot(context.Background(), VisionModel, im)
			}, cfg.Tools)
			if img != nil {
				images = append(images, *img)
			}
			results = append(results, toolResult{Name: tc.Name, Output: out, Image: img})
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: buildToolResults(results), Images: images})
	}
	return fmt.Errorf("reached max %d rounds without finishing", maxRounds)
}
