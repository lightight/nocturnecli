package app

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Version is the CLI version, overridable at build time with
// -ldflags "-X github.com/lightight/nocturnecli/internal/app.Version=...".
var Version = "0.3.0"

const cliHelp = `Nocturne — a terminal coding agent powered by the Nocturne API.

Usage:
  nocturne                  start the interactive TUI
  nocturne -p "task"        run one task non-interactively (auto-runs tools)
  nocturne -m <model>       override the model for this run
  nocturne models           list the models your account can use
  nocturne update           update to the latest release
  nocturne update --check   check for a newer release without installing
  nocturne serve            host the docs, installers, and remote relay
  nocturne --version        print the version
  nocturne --help           show this help

Configuration:
  API key   NOCTURNE_API environment variable, a local .env file, or /key in-app
  Model     default ` + DefaultModel + `; pick with /model, list with /models or -m
  Thinking  /level off · normal · extended (reasoning models)
  Stream    live responses on by default; toggle with /stream
  Config    ~/.config/nocturne/config.json (or the OS equivalent)
`

// Run is the CLI entry point.
func Run(args []string) error {
	// Subcommands.
	if len(args) > 0 && args[0] == "models" {
		cfg := LoadConfig()
		models, def, err := NewClient(cfg).FetchModels(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("default: %s\n\n", def)
		for _, md := range models {
			var tags []string
			if md.Reasoning {
				tags = append(tags, "reasoning")
			}
			if md.Vision {
				tags = append(tags, "vision")
			}
			fmt.Printf("%-34s $%g/$%g  %s\n", md.ID, md.InPrice, md.OutPrice, strings.Join(tags, " "))
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

	for i := 0; i < len(args); i++ {
		switch args[i] {
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
		return runHeadless(cfg, prompt)
	}
	return startTUI(cfg, Version)
}

// runHeadless executes a single task without the TUI: it runs the agent loop,
// auto-accepting tool calls, streaming activity to stderr and the final answer
// to stdout. Suited for scripting and piping.
func runHeadless(cfg *Config, prompt string) error {
	work, _ := os.Getwd()
	client := NewClient(cfg)
	msgs := []ChatMessage{{Role: "user", Content: prompt}}

	const maxRounds = 30
	for round := 0; round < maxRounds; round++ {
		res, err := client.Chat(context.Background(), systemPrompt(work), msgs)
		if err != nil {
			return err
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: res.Text})

		narration, calls := parseResponse(res.Text)
		if len(calls) == 0 {
			fmt.Println(narration)
			return nil
		}
		if narration != "" {
			fmt.Fprintln(os.Stderr, stDim.Render(narration))
		}

		var results []toolResult
		for _, tc := range calls {
			fmt.Fprintln(os.Stderr, stAccent.Render("● ")+tc.summarize())
			results = append(results, toolResult{Name: tc.Name, Output: execute(tc, work)})
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: buildToolResults(results)})
	}
	return fmt.Errorf("reached max %d rounds without finishing", maxRounds)
}
