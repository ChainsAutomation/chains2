package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"chains-automation/operator"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	stateTab = iota
	watchTab
	sendTab
)

type stateResult struct {
	state operator.State
	err   error
}
type profilesResult struct {
	profiles []operator.Profile
	err      error
}
type sendResult struct {
	receipt operator.Receipt
	err     error
}
type watchEnded struct{ err error }
type watchBatch struct {
	records []operator.Record
	ended   *watchEnded
}
type refreshTick struct{}

type tui struct {
	ctx                       context.Context
	client                    *operator.Client
	records                   <-chan tea.Msg
	width, height, tab        int
	state                     operator.State
	visibleState              []operator.StateRow
	stateIndex, watchIndex    int
	collapsed                 map[string]bool
	history                   []operator.Record
	follow                    bool
	all                       bool
	filters                   [2]string
	filter                    textinput.Model
	editingFilter             bool
	detail                    []string
	detailTop                 int
	profiles                  []operator.Profile
	profileIndex              int
	drafts                    map[string]*eventDraft
	pendingState, pendingSend bool
	status                    [3]string
}

func newTUI(ctx context.Context, client *operator.Client, records <-chan tea.Msg) *tui {
	input := newInput("filter")
	return &tui{ctx: ctx, client: client, records: records, width: 80, height: 24, follow: true, collapsed: map[string]bool{}, drafts: map[string]*eventDraft{}, filter: input}
}

func runTUI(ctx context.Context, client *operator.Client, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	studio := make(chan tea.Msg, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(studio)
		err := client.Watch(ctx, 100, func(record operator.Record) error {
			select {
			case studio <- record:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		select {
		case studio <- watchEnded{err}:
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-done }()
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(out), tea.WithAltScreen()}
	if in != os.Stdin {
		options = append(options, tea.WithInput(in))
	}
	_, err := tea.NewProgram(newTUI(ctx, client, studio), options...).Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (m *tui) Init() tea.Cmd {
	return tea.Batch(m.loadState(), m.loadProfiles(), m.nextRecord(), tick())
}
func tick() tea.Cmd { return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return refreshTick{} }) }
func (m *tui) loadState() tea.Cmd {
	if m.pendingState {
		return nil
	}
	m.pendingState = true
	return func() tea.Msg { state, err := m.client.State(m.ctx, nil); return stateResult{state, err} }
}
func (m *tui) loadProfiles() tea.Cmd {
	return func() tea.Msg { profiles, err := m.client.Profiles(m.ctx); return profilesResult{profiles, err} }
}
func (m *tui) nextRecord() tea.Cmd {
	return func() tea.Msg {
		read := func(batch *watchBatch, message tea.Msg, ok bool) bool {
			if !ok {
				batch.ended = &watchEnded{}
				return false
			}
			switch msg := message.(type) {
			case operator.Record:
				batch.records = append(batch.records, msg)
			case watchEnded:
				batch.ended = &msg
				return false
			}
			return true
		}
		var batch watchBatch
		select {
		case message, ok := <-m.records:
			if !read(&batch, message, ok) {
				return batch
			}
		case <-m.ctx.Done():
			return nil
		}
		for len(batch.records) < 64 {
			select {
			case message, ok := <-m.records:
				if !read(&batch, message, ok) {
					return batch
				}
			default:
				return batch
			}
		}
		return batch
	}
}

func (m *tui) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(3, msg.Height)
		m.filter.Width = max(6, m.width-4)
		if m.detail != nil {
			m.detailTop = 0
		}
		return m, nil
	case stateResult:
		m.pendingState = false
		if msg.err != nil {
			m.status[stateTab] = msg.err.Error()
		} else {
			m.state = msg.state
			m.visibleState = nil
			m.status[stateTab] = ""
		}
		return m, nil
	case profilesResult:
		if msg.err != nil {
			m.status[sendTab] = msg.err.Error()
		} else {
			selected := m.profile().Name
			m.profiles = msg.profiles
			m.profileIndex = 0
			for i, p := range m.profiles {
				if p.Name == selected {
					m.profileIndex = i + 1
				}
			}
		}
		return m, nil
	case sendResult:
		m.pendingSend = false
		if msg.err != nil {
			m.status[sendTab] = msg.err.Error()
		} else {
			m.status[sendTab] = "Published " + msg.receipt.Type + " · " + msg.receipt.ID
		}
		return m, nil
	case watchBatch:
		for _, record := range msg.records {
			m.appendRecord(record)
		}
		if msg.ended == nil {
			return m, m.nextRecord()
		}
		if msg.ended.err != nil {
			m.status[watchTab] = msg.ended.err.Error()
		}
		return m, nil
	case refreshTick:
		if m.tab == stateTab {
			return m, tea.Batch(tick(), m.loadState())
		}
		return m, tick()
	case tea.KeyMsg:
		return m, m.key(msg)
	default:
		if m.editingFilter {
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(message)
			return m, cmd
		}
		if m.tab == sendTab && m.detail == nil {
			return m, m.draft().edit(message)
		}
	}
	return m, nil
}

func (m *tui) appendRecord(r operator.Record) {
	if r.Type == "watch.error" {
		m.status[watchTab] = r.Summary
	} else {
		m.status[watchTab] = ""
	}
	if len(m.history) == operator.HistorySize {
		if !m.follow && m.watchIndex > 0 && watchFilter(m.filters[watchTab], m.all).Match(m.history[0]) {
			m.watchIndex--
		}
		copy(m.history, m.history[1:])
		m.history[len(m.history)-1] = r
	} else {
		m.history = append(m.history, r)
	}
}

func (m *tui) key(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.detail != nil {
		lines := m.detailLines()
		maxTop := max(0, len(lines)-max(1, m.height-2))
		switch key {
		case "esc", "enter":
			m.detail = nil
		case "down", "j":
			m.detailTop = min(m.detailTop+1, maxTop)
		case "up", "k":
			m.detailTop = max(0, m.detailTop-1)
		case "pgdown":
			m.detailTop = min(m.detailTop+max(1, m.height-3), maxTop)
		case "pgup":
			m.detailTop = max(0, m.detailTop-max(1, m.height-3))
		}
		return nil
	}
	if m.editingFilter {
		switch key {
		case "enter", "esc":
			if key == "enter" {
				m.filters[m.tab] = m.filter.Value()
				m.stateIndex, m.watchIndex = 0, 0
				m.visibleState = nil
			}
			m.editingFilter = false
			m.filter.Blur()
			return nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return cmd
	}
	switch key {
	case "f1", "f2", "f3":
		m.tab = int(key[1] - '1')
		if m.tab == stateTab {
			return m.loadState()
		}
		if m.tab == sendTab {
			return m.loadProfiles()
		}
		return nil
	}
	if m.tab == sendTab {
		if key == "esc" {
			m.tab = stateTab
			return m.loadState()
		}
		return m.sendKey(msg)
	}
	switch key {
	case "q", "esc":
		return tea.Quit
	case "1", "2", "3":
		m.tab = int(key[0] - '1')
		if m.tab == stateTab {
			return m.loadState()
		}
		if m.tab == sendTab {
			return m.loadProfiles()
		}
	case "/":
		m.editingFilter = true
		m.filter.Width = max(6, m.width-4)
		m.filter.SetValue(m.filters[m.tab])
		m.filter.CursorEnd()
		return m.filter.Focus()
	case "r":
		if m.tab == stateTab {
			return m.loadState()
		}
	case "p":
		if m.tab == watchTab {
			if m.follow {
				m.watchIndex = max(0, len(m.watchRows())-1)
			}
			m.follow = !m.follow
		}
	case "a":
		if m.tab == watchTab {
			m.all = !m.all
			m.watchIndex = 0
		}
	case "up", "down", "j", "k", "pgup", "pgdown", "home", "end":
		m.move(key)
	case "enter", " ":
		m.selectRow(key)
	}
	return nil
}

func (m *tui) move(key string) {
	index := &m.stateIndex
	count := len(m.stateRows())
	if m.tab == watchTab {
		index = &m.watchIndex
		count = len(m.watchRows())
		if m.follow {
			*index = count - 1
			m.follow = false
		}
	}
	switch key {
	case "up", "k":
		*index--
	case "down", "j":
		*index++
	case "pgup":
		*index -= max(1, m.height-3)
	case "pgdown":
		*index += max(1, m.height-3)
	case "home":
		*index = 0
	case "end":
		*index = count - 1
	}
	*index = clamp(*index, count)
}

func (m *tui) selectRow(key string) {
	if m.tab == watchTab {
		rows := m.watchRows()
		if len(rows) == 0 {
			return
		}
		index := clamp(m.watchIndex, len(rows))
		if m.follow {
			index = len(rows) - 1
		}
		m.inspect(rows[index])
		return
	}
	rows := m.stateRows()
	if len(rows) == 0 {
		return
	}
	row := rows[clamp(m.stateIndex, len(rows))]
	if key == " " && row.Kind != "attribute" {
		path := rowPath(row)
		m.collapsed[path] = !m.collapsed[path]
		m.visibleState = nil
		return
	}
	m.inspect(row)
}

func (m *tui) inspect(value any) {
	raw, _ := json.MarshalIndent(value, "", "  ")
	m.detail = strings.Split(string(raw), "\n")
	m.detailTop = 0
}

func (m *tui) detailLines() []string {
	return strings.Split(ansi.Hardwrap(strings.Join(m.detail, "\n"), m.width, true), "\n")
}

func clamp(index, count int) int { return max(0, min(index, count-1)) }
func rowPath(r operator.StateRow) string {
	return strings.Join(slices.DeleteFunc([]string{r.Service, r.Device, r.Attribute}, func(s string) bool { return s == "" }), "/")
}

func (m *tui) stateRows() []operator.StateRow {
	if m.visibleState != nil {
		return m.visibleState
	}
	rows := []operator.StateRow{}
	filter := strings.ToLower(m.filters[stateTab])
	for _, r := range m.state.Rows {
		if filter != "" {
			if !strings.Contains(strings.ToLower(rowPath(r)+" "+r.ServiceName+" "+r.DeviceName+" "+string(r.Value)), filter) {
				continue
			}
		} else if (r.Device != "" && m.collapsed[r.Service]) || (r.Attribute != "" && m.collapsed[r.Service+"/"+r.Device]) {
			continue
		}
		rows = append(rows, r)
	}
	m.visibleState = rows
	return rows
}

func watchFilter(query string, all bool) operator.Filter {
	f := operator.Filter{All: all}
	var text []string
	fields := map[string]*string{"type": &f.Type, "service": &f.Service, "device": &f.Device, "engine": &f.Engine, "rule": &f.Rule, "correlation": &f.Correlation}
	for _, term := range strings.Fields(query) {
		field, value, ok := strings.Cut(term, "=")
		if ptr := fields[field]; ok && ptr != nil {
			*ptr = value
		} else {
			text = append(text, term)
		}
	}
	f.Search = strings.Join(text, " ")
	return f
}
func (m *tui) watchRows() []operator.Record {
	f := watchFilter(m.filters[watchTab], m.all)
	var rows []operator.Record
	for _, r := range m.history {
		if f.Match(r) {
			rows = append(rows, r)
		}
	}
	return rows
}

var accent = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#3150C5", Dark: "#A7BAFF"}).Bold(true)
var muted = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#475569", Dark: "#A1AEC4"})
var selected = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#172554", Dark: "#E0E7FF"}).Background(lipgloss.AdaptiveColor{Light: "#DDE7FF", Dark: "#233358"}).Bold(true)
var alert = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#9A3412", Dark: "#FDBA74"})

func fit(s string, width int) string {
	if width < 1 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}
func cell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = fit(clean(s), width)
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func (m *tui) View() string {
	tabs := []string{"1 State", "2 Watch", "3 Send"}
	if m.tab == sendTab {
		tabs = []string{"F1 State", "F2 Watch", "F3 Send"}
	}
	for i, tab := range tabs {
		if i == m.tab {
			tabs[i] = accent.Render(tab)
		} else {
			tabs[i] = muted.Render(tab)
		}
	}
	nav := strings.Join(tabs, "   ")
	bodyHeight := max(1, m.height-2)
	var body []string
	footer := ""
	switch {
	case m.detail != nil:
		lines := m.detailLines()
		body = lines[min(m.detailTop, len(lines)):]
		footer = "Detail · ↑↓ scroll   Esc back"
	case m.tab == stateTab:
		body = m.stateView(bodyHeight)
		footer = fmt.Sprintf("rev %d · %d rows   / filter   Space fold   Enter details", m.state.Revision, len(m.stateRows()))
	case m.tab == watchTab:
		body = m.watchView(bodyHeight)
		mode := "Live"
		if !m.follow {
			mode = "Paused"
		}
		if m.all {
			mode += " · All"
		}
		footer = mode + "   / filter   p pause   a all   Enter details"
	case m.tab == sendTab:
		body, footer = m.sendView(bodyHeight)
	}
	if m.status[m.tab] != "" && m.detail == nil {
		footer = clean(m.status[m.tab])
	}
	if m.tab != sendTab && m.filters[m.tab] != "" && m.detail == nil {
		footer = "[" + m.filters[m.tab] + "] " + footer
	}
	if m.editingFilter {
		footer = "/ " + m.filter.View()
	}
	lines := []string{fit(nav, m.width)}
	for i := 0; i < bodyHeight; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		lines = append(lines, fit(line, m.width))
	}
	lines = append(lines, muted.Render(fit(footer, m.width)))
	return strings.Join(lines, "\n")
}

func window(count, index, height int) (int, int) {
	start := max(0, min(index-height+1, count-height))
	return start, min(count, start+height)
}

func (m *tui) stateView(height int) []string {
	rows := m.stateRows()
	if len(rows) == 0 {
		text := "No state matches"
		if m.pendingState && m.state.Epoch == "" {
			text = "Loading state…"
		}
		return []string{muted.Render(text)}
	}
	index := clamp(m.stateIndex, len(rows))
	start, end := window(len(rows), index, height)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		r := rows[i]
		branch := "▾ "
		if m.collapsed[rowPath(r)] {
			branch = "▸ "
		}
		text := branch + r.Service
		if r.ServiceName != "" && r.ServiceName != r.Service {
			text += "  " + r.ServiceName
		}
		if r.Device != "" {
			text = "  " + branch + r.Device
			if r.DeviceName != "" && r.DeviceName != r.Device {
				text += "  " + r.DeviceName
			}
		}
		if r.Attribute != "" {
			text = "    " + r.Attribute + "  " + string(r.Value)
		}
		text = clean(text)
		if i == index {
			text = selected.Render(cell(text, m.width))
		}
		lines = append(lines, text)
	}
	return lines
}

func (m *tui) watchView(height int) []string {
	rows := m.watchRows()
	if len(rows) == 0 {
		return []string{muted.Render("Waiting for matching activity…")}
	}
	index := clamp(m.watchIndex, len(rows))
	if m.follow {
		index = len(rows) - 1
	}
	start, end := window(len(rows), index, height)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		r := rows[i]
		subject, detail := activityText(r)
		line := r.At.Local().Format("15:04:05") + "  "
		if m.width >= 76 {
			line += cell(r.Type, 22) + "  " + cell(subject, 22) + "  " + clean(detail)
		} else if m.width >= 45 {
			line += cell(r.Type, 18) + " " + clean(strings.TrimSpace(subject+" "+detail))
		} else {
			line += clean(r.Type + " " + subject)
		}
		if i == index {
			line = selected.Render(cell(line, m.width))
		} else if strings.HasPrefix(r.Type, "watch.") {
			line = alert.Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}
