package main

// Bubble Tea TUI for steam-ctl: menu → multi-select picker → target →
// confirm → steam check → batch run with progress. No gum/fzf/python.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	scMenu screen = iota
	scPick
	scTarget
	scConfirm
	scSteam
	scRun
	scDone
	scList
	scFix
)

var (
	stTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	stHi    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	stDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	stOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	stErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	stWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	stSel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	stBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

type targetOpt struct {
	label string
	path  string
}

type opResult struct {
	appid     string
	name      string
	ok        bool
	cancelled bool
	msg       string
}

type tickMsg struct{}
type opDoneMsg struct{ res opResult }
type progMsg struct{ p MoveProgress }
type steamClosedMsg struct{ err error }

func tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

type model struct {
	opts      Options
	startMode string
	screen    screen
	width     int
	prog      *tea.Program // set after NewProgram; lets copy goroutines Send progress

	apps     []App
	filtered []int
	cursor   int
	selected map[string]bool
	selOrder []string
	filter   string
	mode     string // "move" | "uninstall"

	targets []targetOpt
	tcursor int

	steamPending func() tea.Cmd // what to do after steam closes

	queue   []string
	qidx    int
	results []opResult
	spinner int
	running bool
	runErr  string
	// live copy state for the current op
	cur       MoveProgress
	curName   string
	cancel    context.CancelFunc
	cancelled bool

	listOff  int
	fixText  string
	fixStale bool
	fixDone  string
}

func runTUI(o Options, mode string, preselect []string) error {
	m := &model{
		opts:      o,
		startMode: mode,
		screen:    scMenu,
		selected:  map[string]bool{},
	}
	m.opts.Quiet = true // TUI owns the screen; op stdout is discarded during run
	m.apps = scanApps(o)
	m.refilter()
	if mode == "move" || mode == "uninstall" {
		m.mode = mode
		for _, id := range preselect {
			m.selected[id] = true
			m.selOrder = append(m.selOrder, id)
		}
		if len(preselect) > 0 {
			m.buildTargets()
			m.screen = scTarget
		} else {
			m.screen = scPick
		}
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.prog = p
	_, err := p.Run()
	return err
}

func (m *model) refilter() {
	m.filtered = m.filtered[:0]
	f := strings.ToLower(m.filter)
	for i, a := range m.apps {
		if f == "" || strings.Contains(strings.ToLower(a.Name), f) ||
			strings.Contains(strings.ToLower(a.AppID), f) ||
			strings.Contains(strings.ToLower(a.InstallDir), f) {
			m.filtered = append(m.filtered, i)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

func (m *model) selCount() int { return len(m.selOrder) }

func (m *model) toggle(idx int) {
	a := m.apps[m.filtered[idx]]
	if m.selected[a.AppID] {
		delete(m.selected, a.AppID)
		for i, id := range m.selOrder {
			if id == a.AppID {
				m.selOrder = append(m.selOrder[:i], m.selOrder[i+1:]...)
				break
			}
		}
	} else {
		m.selected[a.AppID] = true
		m.selOrder = append(m.selOrder, a.AppID)
	}
}

func (m *model) selectAll() {
	for _, i := range m.filtered {
		a := m.apps[i]
		if !m.selected[a.AppID] {
			m.selected[a.AppID] = true
			m.selOrder = append(m.selOrder, a.AppID)
		}
	}
}

func (m *model) appByID(id string) App {
	for _, a := range m.apps {
		if a.AppID == id {
			return a
		}
	}
	return App{AppID: id, Name: id}
}

func (m *model) buildTargets() {
	m.targets = nil
	seen := map[string]bool{}
	for _, l := range getLibraries(m.opts) {
		if seen[l] {
			continue
		}
		seen[l] = true
		lrp, _ := filepath.EvalSymlinks(l)
		rrp, _ := filepath.EvalSymlinks(m.opts.SteamRoot)
		label := l
		if lrp == rrp {
			label = fmt.Sprintf("Home      (%s)", l)
		} else {
			label = fmt.Sprintf("External  (%s)", l)
		}
		m.targets = append(m.targets, targetOpt{label, l})
	}
	m.tcursor = 0
}

// ---------- tea.Model ----------

func (m *model) Init() tea.Cmd { return tickCmd() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tickMsg:
		m.spinner++
		if m.screen == scRun || m.screen == scSteam {
			return m, tickCmd()
		}
		return m, nil
	case progMsg:
		m.cur = msg.p
		return m, nil
	case opDoneMsg:
		m.results = append(m.results, msg.res)
		if msg.res.cancelled {
			m.cancelled = true
			m.running = false
			m.screen = scDone
			return m, nil
		}
		m.qidx++
		if m.qidx < len(m.queue) {
			return m, m.runNext()
		}
		m.running = false
		m.screen = scDone
		return m, nil
	case steamClosedMsg:
		if msg.err != nil {
			m.runErr = msg.err.Error()
			return m, nil
		}
		if m.steamPending != nil {
			cmd := m.steamPending
			m.steamPending = nil
			return m, cmd()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" && m.screen != scRun {
		return m, tea.Quit
	}
	switch m.screen {
	case scMenu:
		return m.menuKey(k)
	case scPick:
		return m.pickKey(k)
	case scTarget:
		return m.targetKey(k)
	case scConfirm:
		return m.confirmKey(k)
	case scSteam:
		return m.steamKey(k)
	case scDone:
		if k == "enter" || k == "esc" || k == "q" {
			if m.startMode != "" {
				return m, tea.Quit
			}
			m.screen = scMenu
			m.resetBatch()
		}
		return m, nil
	case scList:
		return m.listKey(k)
	case scFix:
		return m.fixKey(k)
	case scRun:
		switch strings.ToLower(k) {
		case "esc", "q", "ctrl+c":
			if m.running && m.cancel != nil {
				m.cancel()
			}
		}
		return m, nil
	}
	return m, nil
}

// ---------- menu ----------

var menuItems = []string{"Move game(s)", "Uninstall game(s)", "List games", "Fix libraries", "Quit"}
var menuCursor int

func (m *model) menuKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		menuCursor = (menuCursor + len(menuItems) - 1) % len(menuItems)
	case "down", "j":
		menuCursor = (menuCursor + 1) % len(menuItems)
	case "enter":
		switch menuCursor {
		case 0:
			m.mode = "move"
			m.screen = scPick
		case 1:
			m.mode = "uninstall"
			m.screen = scPick
		case 2:
			m.screen = scList
			m.listOff = 0
		case 3:
			m.inspectFix()
			m.screen = scFix
		default:
			return m, tea.Quit
		}
	case "q", "esc":
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) resetBatch() {
	m.selected = map[string]bool{}
	m.selOrder = nil
	m.filter = ""
	m.cursor = 0
	m.queue = nil
	m.results = nil
	m.qidx = 0
	m.runErr = ""
	m.cur = MoveProgress{}
	m.curName = ""
	m.cancel = nil
	m.cancelled = false
	m.refilter()
}

// ---------- picker ----------

func (m *model) pickKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
	case "pgup":
		m.cursor -= 10
		if m.cursor < 0 {
			m.cursor = 0
		}
	case "pgdown":
		m.cursor += 10
		if m.cursor >= len(m.filtered) {
			m.cursor = max(0, len(m.filtered)-1)
		}
	case " ":
		if len(m.filtered) > 0 {
			m.toggle(m.cursor)
		}
	case "ctrl+a":
		m.selectAll()
	case "backspace":
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.refilter()
		}
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.refilter()
		} else if m.startMode != "" {
			return m, tea.Quit
		} else {
			m.screen = scMenu
		}
	case "enter":
		if m.selCount() == 0 {
			return m, nil
		}
		if m.mode == "move" {
			m.buildTargets()
			// skip source lib of first pick when single
			m.screen = scTarget
		} else {
			m.screen = scConfirm
		}
	default:
		if len(k) == 1 && k[0] >= 32 && k[0] < 127 {
			m.filter += k
			m.cursor = 0
			m.refilter()
		}
	}
	return m, nil
}

func (m *model) targetKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.tcursor > 0 {
			m.tcursor--
		}
	case "down", "j":
		if m.tcursor < len(m.targets)-1 {
			m.tcursor++
		}
	case "esc":
		if m.startMode == "move" && len(m.selOrder) > 0 {
			return m, tea.Quit
		}
		m.screen = scPick
	case "enter":
		if len(m.targets) > 0 {
			m.screen = scConfirm
		}
	}
	return m, nil
}

func (m *model) confirmKey(k string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(k) {
	case "y", "enter":
		// steam gate unless dry-run
		if !m.opts.DryRun && isSteamRunning(m.opts) && !m.opts.StopSteam && !m.opts.Force {
			m.screen = scSteam
			return m, tickCmd()
		}
		return m, m.startRun()
	case "n", "esc":
		if m.mode == "move" {
			m.screen = scTarget
		} else {
			m.screen = scPick
		}
	}
	return m, nil
}

func (m *model) steamKey(k string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(k) {
	case "s", "y", "enter":
		m.steamPending = m.startRun
		return m, shutdownSteamCmd(m.opts)
	case "a", "esc", "n":
		m.screen = scConfirm
	}
	return m, nil
}

func shutdownSteamCmd(o Options) tea.Cmd {
	return func() tea.Msg {
		_ = exec.Command("steam", "-shutdown").Run()
		for i := 0; i < 15; i++ {
			if !isSteamRunning(o) {
				break
			}
			time.Sleep(time.Second)
		}
		if isSteamRunning(o) {
			return steamClosedMsg{fmt.Errorf("steam still running after 15s")}
		}
		return steamClosedMsg{}
	}
}

// ---------- run ----------

func (m *model) startRun() tea.Cmd {
	m.queue = append([]string(nil), m.selOrder...)
	m.qidx = 0
	m.results = nil
	m.running = true
	m.cancelled = false
	m.cur = MoveProgress{}
	m.screen = scRun
	if len(m.queue) == 0 {
		m.running = false
		m.screen = scDone
		return nil
	}
	return m.runNext()
}

func (m *model) runNext() tea.Cmd {
	o := m.opts
	o.Force = true // already confirmed on the confirm screen
	appid := m.queue[m.qidx]
	name := m.appByID(appid).Name
	if name == "" {
		name = appid
	}
	m.cur = MoveProgress{}
	m.curName = name
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	send := func(p MoveProgress) {
		if m.prog != nil {
			m.prog.Send(progMsg{p})
		}
	}
	if m.mode == "move" {
		target := m.targets[m.tcursor].path
		return func() tea.Msg {
			err := withSilencedOutput(func() error { return moveWithContext(ctx, o, appid, target, send) })
			res := opResult{appid: appid, name: name, ok: err == nil}
			if err != nil {
				res.msg = err.Error()
				res.cancelled = ctx.Err() != nil
			}
			return opDoneMsg{res}
		}
	}
	return func() tea.Msg {
		err := withSilencedOutput(func() error { return cmdUninstall(o, appid) })
		res := opResult{appid: appid, name: name, ok: err == nil}
		if err != nil {
			res.msg = err.Error()
		}
		return opDoneMsg{res}
	}
}

// withSilencedOutput discards op stdout/stderr so the alt screen stays clean.
func withSilencedOutput(fn func() error) error {
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fn()
	}
	defer dev.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = dev, dev
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	return fn()
}

// ---------- list / fix ----------

func (m *model) listKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.listOff > 0 {
			m.listOff--
		}
	case "down", "j":
		m.listOff++
	case "pgup":
		m.listOff -= 10
		if m.listOff < 0 {
			m.listOff = 0
		}
	case "pgdown":
		m.listOff += 10
	case "esc", "q", "enter":
		if m.startMode != "" {
			return m, tea.Quit
		}
		m.screen = scMenu
	}
	return m, nil
}

func (m *model) inspectFix() {
	data, err := os.ReadFile(m.opts.LibraryVDF)
	if err != nil {
		m.fixText = "No libraryfolders.vdf found: " + m.opts.LibraryVDF
		m.fixStale = false
		return
	}
	var b strings.Builder
	b.WriteString(string(data))
	b.WriteString("\n")
	contentRe := regexp.MustCompile(`"contentid"\s+"([^"]+)"`)
	counts := map[string]int{}
	var order []string
	for _, mm := range contentRe.FindAllStringSubmatch(string(data), -1) {
		if counts[mm[1]] == 0 {
			order = append(order, mm[1])
		}
		counts[mm[1]]++
	}
	for _, id := range order {
		if counts[id] > 1 {
			fmt.Fprintf(&b, "Duplicate contentid %s ×%d (same drive at two paths?)\n", id, counts[id])
		}
	}
	staleRe := regexp.MustCompile(`"path"\s+"([^"]*\/run\/media[^"]*)"`)
	stales := staleRe.FindAllStringSubmatch(string(data), -1)
	m.fixStale = len(stales) > 0
	if len(stales) == 0 {
		b.WriteString("No stale /run/media entry — already clean.\n")
	} else {
		b.WriteString("Stale entries:\n")
		for _, s := range stales {
			b.WriteString("  " + s[1] + "\n")
			if _, err := os.Stat(s[1]); err == nil {
				b.WriteString("  (directory still exists — skipping auto-prune)\n")
				m.fixStale = false
			}
		}
		if m.fixStale {
			b.WriteString("\nPress y to prune + reindex, esc to go back.\n")
		}
	}
	m.fixText = b.String()
	m.fixDone = ""
}

func (m *model) fixKey(k string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(k) {
	case "esc", "q":
		if m.startMode != "" {
			return m, tea.Quit
		}
		m.screen = scMenu
	case "y", "enter":
		if m.fixStale {
			o := m.opts
			o.Force = true
			if err := cmdFixLibraries(o); err != nil {
				m.fixDone = "error: " + err.Error()
			} else {
				m.fixDone = "Pruned stale entry and reindexed."
			}
			m.fixStale = false
		}
	}
	return m, nil
}

// ---------- view ----------

func (m *model) View() string {
	switch m.screen {
	case scMenu:
		return m.viewMenu()
	case scPick:
		return m.viewPick()
	case scTarget:
		return m.viewTarget()
	case scConfirm:
		return m.viewConfirm()
	case scSteam:
		return m.viewSteam()
	case scRun:
		return m.viewRun()
	case scDone:
		return m.viewDone()
	case scList:
		return m.viewList()
	case scFix:
		return m.viewFix()
	}
	return ""
}

func (m *model) viewMenu() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("steam-ctl — keep prefix (Home/External)") + "\n")
	b.WriteString(stDim.Render("choose action") + "\n\n")
	for i, it := range menuItems {
		if i == menuCursor {
			b.WriteString(stSel.Render("▸ "+it) + "\n")
		} else {
			b.WriteString("  " + it + "\n")
		}
	}
	b.WriteString("\n" + stDim.Render("↑↓ navigate · enter select · q quit"))
	return stBox.Render(b.String())
}

func (m *model) viewPick() string {
	var b strings.Builder
	verb := "move (prefix preserved)"
	if m.mode == "uninstall" {
		verb = "uninstall (prefix kept)"
	}
	b.WriteString(stTitle.Render("Select game(s) to "+verb) + "\n")
	b.WriteString(stDim.Render(fmt.Sprintf("filter: %s_  ·  %d selected  ·  %d shown", m.filter, m.selCount(), len(m.filtered))) + "\n\n")
	rows := 15
	start := 0
	if m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	end := min(start+rows, len(m.filtered))
	for i := start; i < end; i++ {
		a := m.apps[m.filtered[i]]
		mark := " "
		if m.selected[a.AppID] {
			mark = stOK.Render("●")
		} else {
			mark = stDim.Render("○")
		}
		prefix := stErr.Render("none")
		if a.HasPrefix {
			prefix = stOK.Render("keep ✓")
		}
		line := fmt.Sprintf("%s %-8s %7s %-6s %-10s %s", mark, trunc(a.AppID, 8), a.HumanSize, "["+trunc(a.ShortLib, 8)+"]", prefix, trunc(a.Name, 30))
		if i == m.cursor {
			line = stSel.Render("▸ " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + stDim.Render("type to filter · space select · ctrl-a all · enter confirm · esc back"))
	return stBox.Render(b.String())
}

func (m *model) viewTarget() string {
	var b strings.Builder
	b.WriteString(stTitle.Render(fmt.Sprintf("Target library for %d game(s)", m.selCount())) + "\n")
	b.WriteString(stDim.Render("single target batch") + "\n\n")
	for i, t := range m.targets {
		if i == m.tcursor {
			b.WriteString(stSel.Render("▸ "+t.label) + "\n")
		} else {
			b.WriteString("  " + t.label + "\n")
		}
	}
	b.WriteString("\n" + stDim.Render("↑↓ navigate · enter select · esc back"))
	return stBox.Render(b.String())
}

func (m *model) viewConfirm() string {
	var b strings.Builder
	var total int64
	for _, id := range m.selOrder {
		total += m.appByID(id).Size
	}
	if m.mode == "move" {
		t := ""
		if len(m.targets) > 0 {
			t = m.targets[m.tcursor].path
		}
		b.WriteString(stTitle.Render(fmt.Sprintf("Move %d game(s) → %s ?", m.selCount(), t)) + "\n\n")
	} else {
		purge := ""
		if m.opts.PurgeCompat {
			purge = stErr.Render(" (PURGE prefix!)")
		}
		b.WriteString(stTitle.Render(fmt.Sprintf("Uninstall %d game(s)? Prefix kept", m.selCount())) + purge + "\n\n")
	}
	for _, id := range m.selOrder {
		a := m.appByID(id)
		b.WriteString(fmt.Sprintf("  • %s (%s) %s\n", trunc(a.Name, 40), a.AppID, a.HumanSize))
	}
	b.WriteString(fmt.Sprintf("\nTotal: %s\n", humanSize(total)))
	if m.opts.DryRun {
		b.WriteString(stWarn.Render("[dry-run] nothing will be executed") + "\n")
	}
	b.WriteString("\n" + stDim.Render("y/enter confirm · n/esc back"))
	return stBox.Render(b.String())
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *model) viewSteam() string {
	var b strings.Builder
	b.WriteString(stWarn.Render("Steam is running.") + "\n\n")
	b.WriteString("Operations need Steam closed.\n\n")
	b.WriteString(stDim.Render("s/enter shutdown & continue · esc abort"))
	if m.runErr != "" {
		b.WriteString("\n" + stErr.Render(m.runErr))
	}
	return stBox.Render(b.String())
}

func (m *model) viewRun() string {
	var b strings.Builder
	verb := "Moving"
	if m.mode == "uninstall" {
		verb = "Uninstalling"
	}
	fr := spinFrames[m.spinner%len(spinFrames)]
	cur := m.curName
	if cur == "" && m.qidx < len(m.queue) {
		cur = m.appByID(m.queue[m.qidx]).Name
	}
	b.WriteString(stTitle.Render(fmt.Sprintf("%s %s %d/%d  %s", fr, verb, m.qidx, len(m.queue), trunc(cur, 28))) + "\n")
	if m.mode == "move" && m.cur.Total > 0 {
		b.WriteString(byteProgressBar(m.cur, 30) + "\n")
		stats := fmt.Sprintf("%s / %s (%.0f%%)", humanSize(m.cur.Done), humanSize(m.cur.Total), m.cur.Pct())
		if m.cur.SpeedBps > 0 {
			stats += fmt.Sprintf("  ·  %s/s", humanSize(int64(m.cur.SpeedBps)))
		}
		if m.cur.ETA > 0 {
			stats += fmt.Sprintf("  ·  ETA %s", fmtETA(m.cur.ETA))
		}
		b.WriteString(stDim.Render(stats) + "\n")
	} else {
		b.WriteString(progressBar(m.qidx, len(m.queue), 30) + "\n")
	}
	b.WriteString("\n")
	// recent results (last 6)
	start := max(0, len(m.results)-6)
	for _, r := range m.results[start:] {
		if r.ok {
			b.WriteString(stOK.Render("✓ "+r.name) + "\n")
		} else {
			b.WriteString(stErr.Render("✗ "+r.name+": "+r.msg) + "\n")
		}
	}
	b.WriteString("\n" + stDim.Render(fmt.Sprintf("game %d of %d · esc cancel", min(m.qidx+1, len(m.queue)), len(m.queue))))
	return stBox.Render(b.String())
}

func byteProgressBar(p MoveProgress, width int) string {
	filled := 0
	if p.Total > 0 {
		filled = int(p.Done) * width / int(p.Total)
		if filled > width {
			filled = width
		}
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s]", bar)
}

func fmtETA(d time.Duration) string {
	s := int(d.Seconds())
	h, m, sec := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

func (m *model) viewDone() string {
	var b strings.Builder
	ok, fail := 0, 0
	for _, r := range m.results {
		if r.ok {
			ok++
		} else {
			fail++
		}
	}
	if fail == 0 && !m.cancelled {
		b.WriteString(stOK.Render(fmt.Sprintf("Done — %d/%d succeeded.", ok, len(m.results))) + "\n\n")
	} else if m.cancelled {
		b.WriteString(stWarn.Render(fmt.Sprintf("Cancelled — %d done, %d remaining skipped.", ok, len(m.queue)-len(m.results))) + "\n")
		b.WriteString(stDim.Render("Current game left intact in its source library; partial target removed.") + "\n\n")
		for _, r := range m.results {
			if !r.ok {
				b.WriteString(stErr.Render("✗ "+r.name+" ("+r.appid+"): "+r.msg) + "\n")
			}
		}
		b.WriteString("\n")
	} else {
		b.WriteString(stWarn.Render(fmt.Sprintf("Done — %d ok, %d failed.", ok, fail)) + "\n\n")
		for _, r := range m.results {
			if !r.ok {
				b.WriteString(stErr.Render("✗ "+r.name+" ("+r.appid+"): "+r.msg) + "\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Reopen Steam — library changes will appear there.\n")
	b.WriteString(stDim.Render("enter/esc continue · q quit"))
	return stBox.Render(b.String())
}

func (m *model) viewList() string {
	var b strings.Builder
	b.WriteString(stTitle.Render(fmt.Sprintf("Installed games (%d)", len(m.apps))) + "\n\n")
	rows := 18
	if m.listOff > len(m.apps)-1 {
		m.listOff = max(0, len(m.apps)-1)
	}
	end := min(m.listOff+rows, len(m.apps))
	for _, a := range m.apps[m.listOff:end] {
		prefix := stDim.Render("none")
		if a.HasPrefix {
			prefix = stOK.Render("keep ✓")
		}
		fmt.Fprintf(&b, "%-8s %7s %-10s %-10s %s\n", trunc(a.AppID, 8), a.HumanSize, trunc(a.ShortLib, 10), prefix, trunc(a.Name, 34))
	}
	b.WriteString("\n" + stDim.Render("↑↓/pgup/pgdn scroll · esc back"))
	return stBox.Render(b.String())
}

func (m *model) viewFix() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("Fix libraries") + "\n\n")
	b.WriteString(m.fixText + "\n")
	if m.fixDone != "" {
		b.WriteString(stOK.Render(m.fixDone) + "\n")
	}
	b.WriteString(stDim.Render("esc back"))
	return stBox.Render(b.String())
}

func progressBar(cur, total, width int) string {
	if total <= 0 {
		return ""
	}
	filled := cur * width / total
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s] %d/%d", bar, cur, total)
}
