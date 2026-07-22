package app

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The /usage dashboard: three navigable tabs (Status · Usage · Stats) with a
// session-activity heatmap, a daily-quota progress bar, and lifetime totals.

// dashTabs are the top-level dashboard tabs, in order.
var dashTabs = []string{"Status", "Usage", "Stats"}

// dashRanges are the Stats date windows cycled with `r`.
var dashRanges = []struct {
	label string
	days  int // 0 = all time
}{
	{"All time", 0},
	{"Last 7 days", 7},
	{"Last 30 days", 30},
}

// heatGlyph/heatColor define the 5 intensity levels of the activity heatmap.
var heatGlyph = []string{"·", "░", "▒", "▓", "█"}
var heatColor = []lipgloss.Color{
	lipgloss.Color("#3b3b45"), // 0 — empty
	lipgloss.Color("#7a4a2a"), // 1
	lipgloss.Color("#b06a34"), // 2
	lipgloss.Color("#d98a4a"), // 3
	lipgloss.Color("#f0a860"), // 4
}

// dashStats are the session-derived aggregates shown on the Stats tab.
type dashStats struct {
	sessions       int
	activeDays     int
	rangeDays      int
	mostActive     string
	longestSession time.Duration
	longestStreak  int
	currentStreak  int
	favModel       string
	activity       map[string]int // "2006-01-02" → weight
}

// computeStats derives the Stats-tab aggregates from all saved sessions,
// filtered to the selected date window.
func computeStats(sessions []Session, days int) dashStats {
	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}
	st := dashStats{activity: map[string]int{}}
	modelCount := map[string]int{}
	var first time.Time
	for _, s := range sessions {
		when := s.Updated
		if when.IsZero() {
			when = s.Started
		}
		if days > 0 && when.Before(cutoff) {
			continue
		}
		st.sessions++
		key := when.Format("2006-01-02")
		w := countUserMsgs(s.Messages)
		if w == 0 {
			w = 1
		}
		st.activity[key] += w
		if s.Model != "" {
			modelCount[displayModel(s.Model)]++
		}
		if d := s.Updated.Sub(s.Started); d > st.longestSession {
			st.longestSession = d
		}
		if first.IsZero() || when.Before(first) {
			first = when
		}
	}

	st.activeDays = len(st.activity)
	switch {
	case days > 0:
		st.rangeDays = days
	case !first.IsZero():
		st.rangeDays = int(time.Since(first).Hours()/24) + 1
	}

	// Most active day + favourite model.
	best := -1
	for day, n := range st.activity {
		if n > best {
			best = n
			if t, err := time.Parse("2006-01-02", day); err == nil {
				st.mostActive = t.Format("Jan 2")
			}
		}
	}
	fav, favN := "", -1
	for md, n := range modelCount {
		if n > favN {
			favN, fav = n, md
		}
	}
	st.favModel = fav

	st.longestStreak, st.currentStreak = streaks(st.activity)
	return st
}

// streaks returns the longest and current consecutive-active-day runs.
func streaks(activity map[string]int) (longest, current int) {
	if len(activity) == 0 {
		return 0, 0
	}
	days := make([]time.Time, 0, len(activity))
	for k := range activity {
		if t, err := time.Parse("2006-01-02", k); err == nil {
			days = append(days, t)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	run := 1
	longest = 1
	for i := 1; i < len(days); i++ {
		if days[i].Sub(days[i-1]) <= 36*time.Hour { // consecutive calendar days
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
	}
	// Current streak: only counts if the most recent active day is today/yesterday.
	last := days[len(days)-1]
	if daysApart(last, time.Now()) <= 1 {
		current = 1
		for i := len(days) - 1; i > 0; i-- {
			if days[i].Sub(days[i-1]) <= 36*time.Hour {
				current++
			} else {
				break
			}
		}
	}
	return longest, current
}

func daysApart(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	da := time.Date(ay, am, ad, 0, 0, 0, 0, time.Local)
	db := time.Date(by, bm, bd, 0, 0, 0, 0, time.Local)
	return int(math.Abs(db.Sub(da).Hours()) / 24)
}

// --- rendering -------------------------------------------------------------

// dashboardBody renders the scrollable dashboard content for the viewport.
func (m *tuiModel) dashboardBody() string {
	var b strings.Builder
	b.WriteString(m.dashTabsHeader())
	b.WriteString("\n\n")
	switch m.dashTab {
	case 0:
		b.WriteString(m.renderStatus())
	case 1:
		b.WriteString(m.renderUsage())
	default:
		b.WriteString(m.renderStats())
	}
	return b.String()
}

// dashFooter is the hint line shown beneath the dashboard viewport.
func (m *tuiModel) dashFooter() string {
	rkey := "r retry"
	if m.dashTab == 2 {
		rkey = "r cycle range"
	}
	hint := stHint.Render(fmt.Sprintf("  ← → switch tab · %s · ctrl+s copy · esc close · scroll ↕", rkey))
	if m.dashNote != "" {
		return "  " + m.dashNote + "\n" + hint
	}
	return hint
}

func (m *tuiModel) dashTabsHeader() string {
	var cells []string
	for i, t := range dashTabs {
		if i == m.dashTab {
			cells = append(cells, stTabOn.Render(t))
		} else {
			cells = append(cells, stTabOff.Render(t))
		}
	}
	return "  " + strings.Join(cells, " ")
}

// kv formats an aligned "label   value" row for the Status/Usage tabs.
func kv(label, value string) string {
	return "  " + stLabel.Render(padRight(label, 15)) + value
}

func (m *tuiModel) renderStatus() string {
	var b strings.Builder
	ver := m.ver
	acct := stDim.Render("API key · quota only")
	if m.usage != nil && m.usage.Authenticated {
		acct = stOK.Render("authenticated ✓")
	} else if m.usageErr != nil {
		acct = stErr.Render("unavailable")
	}
	remote := "off"
	if m.remote != nil {
		remote = stAccent.Render(m.remote.url)
	}
	b.WriteString(kv("Version", stAccent.Render(ver)) + "\n")
	b.WriteString(kv("Session ID", stDim.Render(m.sessionID)) + "\n")
	b.WriteString(kv("cwd", prettyPath(m.work)) + "\n")
	b.WriteString(kv("Model", stAccent.Render(displayModel(m.cfg.Model))) + "\n")
	b.WriteString(kv("Thinking", levelLabel(m.cfg.Level)) + "\n")
	b.WriteString(kv("Approvals", permLabel(m.cfg.Perm)) + "\n")
	b.WriteString(kv("Streaming", boolLabel(m.cfg.Stream, "on", "off")) + "\n")
	b.WriteString(kv("API key", stDim.Render(orDash(m.cfg.MaskedKey()))) + "\n")
	b.WriteString(kv("Remote", remote) + "\n")
	b.WriteString(kv("Account", acct) + "\n")
	b.WriteString(kv("Config", stDim.Render(prettyPath(ConfigPath()))))
	return b.String()
}

func (m *tuiModel) renderUsage() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("  Session") + "\n")
	b.WriteString(kv("Model", stAccent.Render(displayModel(m.cfg.Model))) + "\n")
	b.WriteString(kv("Duration", stAccent.Render(humanDur(time.Since(m.sessionStart)))) + "\n")
	b.WriteString(kv("Messages", stAccent.Render(strconv.Itoa(countUserMsgs(m.messages)))) + "\n")
	b.WriteString(kv("Tokens", stAccent.Render(commas(m.tokens))) + "\n")
	changes := stOK.Render("+"+strconv.Itoa(m.linesAdded)) + stDim.Render(" / ") + stErr.Render("-"+strconv.Itoa(m.linesRemoved)) + stDim.Render(" lines")
	b.WriteString(kv("Code changes", changes) + "\n\n")

	b.WriteString(stTitle.Render("  Account") + "\n")

	// Daily quota progress bar. The /credits endpoint gives the richest data,
	// but it's cookie-gated; the same daily used/cap also rides along on every
	// chat response (m.lastQuota), so the bar still works with just an API key.
	used, capacity, unlimited, resetAt, haveDaily := m.dailyQuota()
	if haveDaily {
		if unlimited {
			b.WriteString(kv("Daily usage", stOK.Render("unlimited")) + "\n")
		} else {
			frac := 0.0
			if capacity > 0 {
				frac = float64(used) / float64(capacity)
			}
			pct := int(math.Round(frac * 100))
			bar := progressBar(frac, 24)
			usedCap := stDim.Render(fmt.Sprintf("%s / %s", commas64(used), commas64(capacity)))
			b.WriteString(kv("Daily usage", fmt.Sprintf("%s %s   %s", bar, stAccent.Render(fmt.Sprintf("%d%%", pct)), usedCap)) + "\n")
			if resetAt > 0 {
				b.WriteString(kv("Resets", stDim.Render("in "+humanDur(time.Until(time.UnixMilli(resetAt))))) + "\n")
			}
		}
	}

	// Lifetime / weekly totals come only from the (cookie-gated) usage+credits
	// endpoints. When they're unavailable, fall back to this session's counters.
	if m.usage != nil && m.usage.Authenticated {
		u := m.usage
		b.WriteString(kv("All-time", stAccent.Render(abbrev(u.AllTokens)+" tokens")+stDim.Render(fmt.Sprintf(" · $%.2f spent", u.AllCost))) + "\n")
		if m.credits != nil {
			credit := "$" + abbrevF(m.credits.Dollars)
			if m.credits.Unlimited {
				credit = "unlimited"
			}
			b.WriteString(kv("Credits", stAccent.Render(credit)+stDim.Render(" balance")) + "\n")
		}
		b.WriteString(kv("This week", stAccent.Render(commas64(u.WeekTokens)+" tokens")+stDim.Render(fmt.Sprintf(" · $%.2f", u.WeekCost))))
		return b.String()
	}

	// No account-wide data — explain why, and show what we do have.
	if m.usageErr != nil {
		b.WriteString(kv("Account", stErr.Render(oneLine(m.usageErr.Error(), 70))) + "\n")
		b.WriteString(stHint.Render("  press r to retry"))
		return b.String()
	}
	if m.usage == nil || m.credits == nil {
		b.WriteString(stHint.Render("  loading account usage…"))
		return b.String()
	}
	b.WriteString(kv("Session tokens", stAccent.Render(commas(m.tokens))) + "\n")
	b.WriteString(stHint.Render("  lifetime totals need a web session — sign in at nocturne.lol"))
	return b.String()
}

// dailyQuota resolves the daily token quota, preferring the /credits endpoint
// and falling back to the quota attached to the latest chat response.
func (m *tuiModel) dailyQuota() (used, capacity int64, unlimited bool, resetAt int64, ok bool) {
	if m.credits != nil && m.credits.Authenticated {
		d := m.credits.Daily
		if m.credits.Unlimited || d.Cap > 0 {
			return d.Used, d.Cap, m.credits.Unlimited, d.ResetAt, true
		}
	}
	if q := m.lastQuota; q.Unlimited || q.Cap > 0 {
		return int64(q.Used), int64(q.Cap), q.Unlimited, 0, true
	}
	return 0, 0, false, 0, false
}

func (m *tuiModel) renderStats() string {
	rng := dashRanges[m.dashRange]
	st := computeStats(m.dashSessions, rng.days)

	var b strings.Builder
	b.WriteString(m.renderHeatmap(st.activity))
	b.WriteString("\n\n")

	// Range toggle.
	var toggle []string
	for i, r := range dashRanges {
		if i == m.dashRange {
			toggle = append(toggle, stPrim.Bold(true).Render(r.label))
		} else {
			toggle = append(toggle, stDim.Render(r.label))
		}
	}
	b.WriteString("  " + strings.Join(toggle, stDim.Render(" · ")) + "\n\n")

	// Two-column stat grid.
	totalTokens := "—"
	if m.usage != nil {
		switch rng.days {
		case 7:
			totalTokens = abbrev(m.usage.WeekTokens)
		default:
			totalTokens = abbrev(m.usage.AllTokens)
		}
	}
	left := []string{
		statLine("Favorite model", orDash(st.favModel), colAccent),
		statLine("Sessions", strconv.Itoa(st.sessions), colPrimary),
		statLine("Active days", fmt.Sprintf("%d%s", st.activeDays, dimFrac(st.rangeDays)), colPrimary),
		statLine("Most active day", orDash(st.mostActive), colPrimary),
	}
	right := []string{
		statLine("Total tokens", totalTokens, colPrimary),
		statLine("Longest session", orDash(humanDurZero(st.longestSession)), colPrimary),
		statLine("Longest streak", fmt.Sprintf("%d day%s", st.longestStreak, plural(st.longestStreak)), colPrimary),
		statLine("Current streak", fmt.Sprintf("%d day%s", st.currentStreak, plural(st.currentStreak)), colPrimary),
	}
	for i := range left {
		b.WriteString("  " + padVisible(left[i], 40) + right[i] + "\n")
	}

	// Fun comparison.
	if m.usage != nil && m.usage.AllTokens > 0 {
		b.WriteString("\n  " + stAccent.Render(tokenComparison(m.usage.AllTokens)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// statLine renders "Label: value" with a bold label and coloured value.
func statLine(label, value string, col lipgloss.Color) string {
	return stLabel.Render(label+": ") + lipgloss.NewStyle().Foreground(col).Render(value)
}

func dimFrac(total int) string {
	if total <= 0 {
		return ""
	}
	return stDim.Render("/" + strconv.Itoa(total))
}

// renderHeatmap draws a GitHub-style activity grid ending at today.
func (m *tuiModel) renderHeatmap(activity map[string]int) string {
	weeks := (m.width - 10) / 2
	if weeks > 30 {
		weeks = 30
	}
	if weeks < 10 {
		weeks = 10
	}

	maxN := 0
	for _, n := range activity {
		if n > maxN {
			maxN = n
		}
	}

	now := time.Now()
	// Start at the Sunday that begins the earliest visible week.
	startSunday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -int(now.Weekday())-7*(weeks-1))

	// Month header row: label each column where its month changes.
	header := make([]rune, weeks*2)
	for i := range header {
		header[i] = ' '
	}
	lastMonth := time.Month(0)
	for c := 0; c < weeks; c++ {
		wk := startSunday.AddDate(0, 0, c*7)
		if wk.Month() != lastMonth {
			lastMonth = wk.Month()
			lbl := wk.Format("Jan")
			for k := 0; k < len(lbl) && c*2+k < len(header); k++ {
				header[c*2+k] = rune(lbl[k])
			}
		}
	}

	var b strings.Builder
	b.WriteString("      " + stDim.Render(string(header)) + "\n")

	rowLabels := map[int]string{1: "Mon", 3: "Wed", 5: "Fri"}
	for r := 0; r < 7; r++ {
		label := "   "
		if l, ok := rowLabels[r]; ok {
			label = l
		}
		b.WriteString(stDim.Render("  "+label) + " ")
		for c := 0; c < weeks; c++ {
			day := startSunday.AddDate(0, 0, c*7+r)
			if day.After(now) {
				b.WriteString("  ")
				continue
			}
			lvl := heatLevel(activity[day.Format("2006-01-02")], maxN)
			b.WriteString(lipgloss.NewStyle().Foreground(heatColor[lvl]).Render(heatGlyph[lvl]) + " ")
		}
		b.WriteString("\n")
	}

	// Legend.
	legend := stDim.Render("Less ")
	for lvl := 1; lvl < 5; lvl++ {
		legend += lipgloss.NewStyle().Foreground(heatColor[lvl]).Render(heatGlyph[lvl]) + " "
	}
	legend += stDim.Render("More")
	b.WriteString("      " + legend)
	return b.String()
}

func heatLevel(count, max int) int {
	if count <= 0 || max <= 0 {
		return 0
	}
	switch {
	case count >= max:
		return 4
	case count >= (max*3+3)/4:
		return 4
	case count >= (max*2+3)/4:
		return 3
	case count >= (max+3)/4:
		return 2
	default:
		return 1
	}
}

// progressBar renders a [████░░░░] bar of the given cell width.
func progressBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	// Always show at least a sliver when there's any usage at all.
	if filled == 0 && frac > 0 {
		filled = 1
	}
	bar := lipgloss.NewStyle().Foreground(colPrimary).Render(strings.Repeat("█", filled)) +
		stDim.Render(strings.Repeat("░", width-filled))
	return "[" + bar + "]"
}

// --- number / time formatting ----------------------------------------------

func abbrev(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e9)) + "b"
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "m"
	case n >= 1_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e3)) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

func abbrevF(f float64) string {
	if f >= 1000 {
		return abbrev(int64(f))
	}
	return trimZero(fmt.Sprintf("%.2f", f))
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

func commas64(n int64) string { return commas(int(n)) }

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	mnt := int(d / time.Minute)
	var parts []string
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
	}
	if h > 0 {
		parts = append(parts, strconv.Itoa(h)+"h")
	}
	if mnt > 0 || len(parts) == 0 {
		parts = append(parts, strconv.Itoa(mnt)+"m")
	}
	return strings.Join(parts, " ")
}

func humanDurZero(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return humanDur(d)
}

func boolLabel(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// padVisible right-pads a possibly-styled string to n visible columns.
func padVisible(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// tokenComparison returns a playful "Nx more tokens than <work>" line.
func tokenComparison(tokens int64) string {
	refs := []struct {
		name   string
		tokens int64
	}{
		{"a text message", 30},
		{"this README", 3_000},
		{"the U.S. Constitution", 10_000},
		{"a Harry Potter book", 120_000},
		{"Les Misérables", 700_000},
		{"the King James Bible", 900_000},
		{"the whole Harry Potter series", 1_500_000},
		{"the Lord of the Rings trilogy", 2_200_000},
	}
	// Largest reference the total still beats by at least 2×.
	best := -1
	for i, r := range refs {
		if tokens >= 2*r.tokens {
			best = i
		}
	}
	if best < 0 {
		return fmt.Sprintf("That's about as many tokens as %s.", refs[0].name)
	}
	ratio := int(math.Round(float64(tokens) / float64(refs[best].tokens)))
	return fmt.Sprintf("You've used ~%dx more tokens than %s.", ratio, refs[best].name)
}
