package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"
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
	stLabel    = lipgloss.NewStyle().Bold(true)

	// Dashboard tabs: the active tab gets a filled accent chip.
	stTabOn  = lipgloss.NewStyle().Background(colAccent).Foreground(lipgloss.Color("#1a1a22")).Bold(true).Padding(0, 1)
	stTabOff = lipgloss.NewStyle().Foreground(colDim).Padding(0, 1)
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
		stDim.Render("• ") + stAccent.Render("/help") + stDim.Render(" for commands · ") + stAccent.Render("Esc") + stDim.Render(" interrupts · ") + stAccent.Render("Ctrl+C") + stDim.Render(" twice quits."),
	}
	return strings.Join(rows, "\n")
}

// moonFrames is the waxing/waning moon animation used for the thinking spinner
// and other in-progress indicators — on brand for Nocturne.
var moonFrames = []string{"🌑", "🌒", "🌓", "🌔", "🌕", "🌖", "🌗", "🌘"}

// moonAt returns the moon frame at animation counter i (wrapping).
func moonAt(i int) string { return moonFrames[((i%len(moonFrames))+len(moonFrames))%len(moonFrames)] }

// partyPalette is the flowing rainbow used by /party mode.
var partyPalette = []lipgloss.Color{
	lipgloss.Color("#ff5555"), // red
	lipgloss.Color("#ffb86c"), // orange
	lipgloss.Color("#f1fa8c"), // yellow
	lipgloss.Color("#50fa7b"), // green
	lipgloss.Color("#8be9fd"), // cyan
	lipgloss.Color("#7aa2f7"), // blue
	lipgloss.Color("#bd93f9"), // violet
	lipgloss.Color("#ff79c6"), // pink
}

// rainbowAt returns the palette colour at position i (wrapping), so callers can
// walk it with an animation counter for a flowing effect.
func rainbowAt(i int) lipgloss.Color {
	n := len(partyPalette)
	return partyPalette[((i%n)+n)%n]
}

// rainbowText colours each visible rune of s with a successive palette colour,
// offset by the animation counter so the colours appear to flow along the text.
func rainbowText(s string, offset int) string {
	var b strings.Builder
	i := 0
	for _, r := range s {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(lipgloss.NewStyle().Foreground(rainbowAt(offset + i)).Render(string(r)))
		i++
	}
	return b.String()
}

// renderUser formats an echoed user message for the scrollback.
func renderUser(text string, nImages, width int) string {
	head := stUser.Render("›") + " "
	body := text
	if nImages > 0 {
		tag := stAccent.Render(fmt.Sprintf(" [%d image%s]", nImages, plural(nImages)))
		if body == "" {
			body = stDim.Render("(image)")
		}
		body += tag
	}

	// The textarea wraps long prompts while editing; do the same after the
	// message is submitted so long user requests don't run off the terminal.
	// Account for the leading prompt marker and indent continuation lines to
	// keep multi-line messages visually grouped under the request.
	contentWidth := width - lipgloss.Width(head)
	if contentWidth < 8 {
		contentWidth = 8
	}
	body = wrap.String(body, contentWidth)
	if strings.Contains(body, "\n") {
		body = strings.ReplaceAll(body, "\n", "\n  ")
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
	switch canonicalTool(tc.Name) {
	case "read_file", "write_file", "edit_file", "delete":
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
	case "rename":
		return "(" + argStr(tc.Args, "from") + " → " + argStr(tc.Args, "to") + ")"
	case "import_github":
		return "(" + repoArg(tc.Args) + ")"
	case "ask":
		return " " + oneLine(argStr(tc.Args, "question"), 60)
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
