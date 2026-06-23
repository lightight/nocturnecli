package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Nocturne palette — warm amber primary (echoing the inspiration) with a
// nocturnal violet accent.
var (
	colPrimary = lipgloss.Color("#E0A458") // amber
	colAccent  = lipgloss.Color("#A78BFA") // violet
	colPink    = lipgloss.Color("#F0ABFC")
	colDim     = lipgloss.Color("#8A8A99")
	colGreen   = lipgloss.Color("#7BD88F")
	colRed     = lipgloss.Color("#F2777A")
	colBlue    = lipgloss.Color("#7AA2F7")
)

var (
	stTitle    = lipgloss.NewStyle().Foreground(colPrimary).Bold(true)
	stAccent   = lipgloss.NewStyle().Foreground(colAccent)
	stDim      = lipgloss.NewStyle().Foreground(colDim)
	stUser     = lipgloss.NewStyle().Foreground(colPink).Bold(true)
	stToolName = lipgloss.NewStyle().Foreground(colBlue).Bold(true)
	stToolArg  = lipgloss.NewStyle().Foreground(colDim)
	stResult   = lipgloss.NewStyle().Foreground(colDim)
	stOK       = lipgloss.NewStyle().Foreground(colGreen)
	stErr      = lipgloss.NewStyle().Foreground(colRed)
	stBorder   = lipgloss.NewStyle().Foreground(colPrimary)
	stHint     = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	stPrim     = lipgloss.NewStyle().Foreground(colPrimary)
)

// moonArt is the ASCII crescent-moon mascot shown in the banner — a thin
// waxing crescent lit on the right. Uses only CP437-safe block/shade glyphs
// plus ASCII stars so it renders everywhere.
const moonArt = `  *     ▓█
       ▒▓█
 ·    ░▒▓█
       ▒▓█
  *     ▓█`

// banner renders the welcome box shown on startup, with the ASCII moon to the
// left of the session info — in the spirit of the Claude CLI's mascot.
func banner(version, model, cwd string) string {
	moon := stPrim.Render(moonArt)

	info := lipgloss.JoinVertical(lipgloss.Left,
		stTitle.Render("◗ Nocturne")+stDim.Render("  coding agent"),
		stDim.Render("v"+version),
		"",
		stDim.Render("model  ")+stAccent.Render(model),
		stDim.Render("cwd    ")+cwd,
	)

	body := lipgloss.JoinHorizontal(lipgloss.Center, moon, stDim.Render("   "), info)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPrimary).
		Padding(1, 3)

	return box.Render(body)
}

// tips is the short help shown under the banner.
func tips() string {
	rows := []string{
		stDim.Render("• Ask me to read, edit, search files or run commands."),
		stDim.Render("• Paste images with ") + stAccent.Render("Ctrl+V") + stDim.Render(", or drop a file path inline."),
		stDim.Render("• ") + stAccent.Render("/help") + stDim.Render(" for commands · ") + stAccent.Render("Esc") + stDim.Render(" interrupts · ") + stAccent.Render("Ctrl+C") + stDim.Render(" quits."),
	}
	return strings.Join(rows, "\n")
}

// renderUser formats an echoed user message for the scrollback.
func renderUser(text string, nImages int) string {
	head := stUser.Render("›") + " "
	body := text
	if nImages > 0 {
		tag := stAccent.Render(fmt.Sprintf(" [%d image%s]", nImages, plural(nImages)))
		if body == "" {
			body = stDim.Render("(image)")
		}
		body += tag
	}
	return head + body
}

// renderToolCall formats the "● tool(args)" header.
func renderToolCall(tc ToolCall) string {
	return stAccent.Render("●") + " " + stToolName.Render(toolDisplayName(tc.Name)) +
		stToolArg.Render(toolArgsPreview(tc))
}

// renderToolResult formats the "└ result" follow-up line(s).
func renderToolResult(output string) string {
	first := output
	rest := ""
	if i := strings.IndexByte(output, '\n'); i >= 0 {
		first = output[:i]
		n := strings.Count(output[i+1:], "\n") + 1
		rest = stDim.Render(fmt.Sprintf("  … +%d line%s", n, plural(n)))
	}
	style := stResult
	switch {
	case strings.HasPrefix(output, "Error:") || strings.HasPrefix(output, "EDIT FAILED") || strings.Contains(first, "FAILED:"):
		style = stErr
	case strings.HasPrefix(output, "EDIT APPLIED") || strings.HasPrefix(output, "Wrote ") || strings.HasPrefix(output, "Edited "):
		style = stOK
	}
	line := stDim.Render("  └ ") + style.Render(oneLine(first, 120))
	if rest != "" {
		line += "\n" + rest
	}
	return line
}

func toolDisplayName(name string) string { return name }

func toolArgsPreview(tc ToolCall) string {
	switch tc.Name {
	case "read_file", "write_file", "edit_file":
		return "(" + argStr(tc.Args, "path") + ")"
	case "list_dir":
		p := argStr(tc.Args, "path")
		if p == "" {
			p = "."
		}
		return "(" + p + ")"
	case "search":
		return "(" + argStr(tc.Args, "pattern") + ")"
	case "run_command":
		return " " + oneLine(argStr(tc.Args, "command"), 80)
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
