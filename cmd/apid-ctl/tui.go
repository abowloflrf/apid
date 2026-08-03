package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/abowloflrf/apid/session"
)

type tuiModel struct {
	table    table.Model
	sessions []session.Session
	sources  []session.SourceInfo
	width    int
	height   int
}

type tableField int

const (
	fieldTime tableField = iota
	fieldShortTime
	fieldTool
	fieldID
	fieldModel
	fieldCWD
	fieldTokens
	fieldCache
	fieldTitle
)

type tableLayout struct {
	columns []table.Column
	fields  []tableField
}

func newTUIModel(sources []session.SourceInfo, sessions []session.Session) tuiModel {
	m := tuiModel{sources: sources, sessions: sessions, width: 120, height: 24}
	layout := layoutForWidth(m.width)
	m.rebuildTable(layout, 0)
	return m
}

func (m tuiModel) Init() tea.Cmd { return nil }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = max(20, msg.Width)
		m.height = max(8, msg.Height)
		cursor := m.table.Cursor()
		layout := layoutForWidth(m.width)
		m.rebuildTable(layout, cursor)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m tuiModel) View() tea.View {
	muted := lipgloss.NewStyle().Faint(true)
	selected := ""
	if cursor := m.table.Cursor(); cursor >= 0 && cursor < len(m.sessions) {
		s := m.sessions[cursor]
		selected = fmt.Sprintf("选择: %s · %s · %s", s.ID, shortHome(s.CWD), clean(s.Title))
	}
	content := strings.Join([]string{
		clip(sourceLine(m.sources), m.width),
		m.table.View(),
		muted.Render(clip(selected, m.width)),
		clip(summaryLine(m.sessions), m.width),
		muted.Render(clip("↑/↓ j/k 移动 · PgUp/PgDn 翻页 · g/G 首尾 · q/Esc 退出", m.width)),
	}, "\n")
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "apid-ctl sessions"
	return view
}

func (m tuiModel) tableHeight() int {
	return max(3, m.height-4)
}

func (m *tuiModel) rebuildTable(layout tableLayout, cursor int) {
	m.table = table.New(
		table.WithColumns(layout.columns),
		table.WithRows(styledRowsForLayout(m.sessions, layout)),
		table.WithFocused(true),
		table.WithWidth(m.width),
		table.WithHeight(m.tableHeight()),
		table.WithStyles(sessionTableStyles()),
	)
	m.table.SetCursor(cursor)
}

func layoutForWidth(width int) tableLayout {
	switch {
	case width >= 128:
		return makeLayout(width,
			[]table.Column{
				{Title: "时间", Width: 16},
				{Title: "T", Width: 2},
				{Title: "ID", Width: 8},
				{Title: "模型", Width: 20},
				{Title: "工作目录", Width: 18},
				{Title: "Tokens", Width: 7},
				{Title: "缓存", Width: 7},
			},
			[]tableField{fieldTime, fieldTool, fieldID, fieldModel, fieldCWD, fieldTokens, fieldCache})
	case width >= 94:
		return makeLayout(width,
			[]table.Column{
				{Title: "时间", Width: 16},
				{Title: "T", Width: 2},
				{Title: "模型", Width: 18},
				{Title: "工作目录", Width: 16},
				{Title: "Tokens", Width: 7},
				{Title: "缓存", Width: 7},
			},
			[]tableField{fieldTime, fieldTool, fieldModel, fieldCWD, fieldTokens, fieldCache})
	case width >= 70:
		return makeLayout(width,
			[]table.Column{
				{Title: "时间", Width: 16},
				{Title: "T", Width: 2},
				{Title: "模型", Width: 16},
				{Title: "Tokens", Width: 7},
				{Title: "缓存", Width: 6},
			},
			[]tableField{fieldTime, fieldTool, fieldModel, fieldTokens, fieldCache})
	default:
		return makeLayout(width,
			[]table.Column{{Title: "时间", Width: 11}, {Title: "T", Width: 2}, {Title: "Tokens", Width: 7}},
			[]tableField{fieldShortTime, fieldTool, fieldTokens})
	}
}

func makeLayout(width int, fixed []table.Column, fields []tableField) tableLayout {
	used := 0
	for _, column := range fixed {
		used += column.Width
	}
	padding := (len(fixed) + 1) * 2
	titleWidth := max(1, width-used-padding)
	columns := append(append([]table.Column(nil), fixed...), table.Column{Title: "标题", Width: titleWidth})
	return tableLayout{columns: columns, fields: append(append([]tableField(nil), fields...), fieldTitle)}
}

func rowsForLayout(sessions []session.Session, layout tableLayout) []table.Row {
	rows := make([]table.Row, 0, len(sessions))
	for _, s := range sessions {
		row := make(table.Row, 0, len(layout.fields))
		for _, field := range layout.fields {
			switch field {
			case fieldTime:
				row = append(row, formatTime(s.UpdatedAt))
			case fieldShortTime:
				row = append(row, formatShortTime(s.UpdatedAt))
			case fieldTool:
				row = append(row, toolLabel(s.Tool))
			case fieldID:
				row = append(row, shortID(s.ID))
			case fieldModel:
				row = append(row, clean(s.Model))
			case fieldCWD:
				row = append(row, shortHome(s.CWD))
			case fieldTokens:
				row = append(row, formatTokens(s.TokensUsed))
			case fieldCache:
				row = append(row, formatCache(s.CacheHitRate))
			case fieldTitle:
				row = append(row, clean(s.Title))
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func styledRowsForLayout(sessions []session.Session, layout tableLayout) []table.Row {
	rows := rowsForLayout(sessions, layout)
	for i, s := range sessions {
		for j, field := range layout.fields {
			value := rows[i][j]
			if field == fieldTokens || field == fieldCache {
				value = strings.Repeat(" ", max(0, layout.columns[j].Width-len(value))) + value
			}
			rows[i][j] = sessionCellStyle(s, field).Render(value)
		}
	}
	return rows
}

func sessionTableStyles() table.Styles {
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Foreground(lipgloss.Cyan)
	styles.Selected = lipgloss.NewStyle().Reverse(true)
	return styles
}

func sessionCellStyle(s session.Session, field tableField) lipgloss.Style {
	style := lipgloss.NewStyle()
	switch field {
	case fieldTime, fieldShortTime:
		return style.Faint(true)
	case fieldTool:
		switch s.Tool {
		case session.ToolCodex:
			return style.Foreground(lipgloss.Magenta)
		case session.ToolClaude:
			return style.Foreground(lipgloss.Yellow)
		case session.ToolPi:
			return style.Foreground(lipgloss.Green)
		case session.ToolOpenCode:
			return style.Foreground(lipgloss.Blue)
		default:
			return style.Foreground(lipgloss.White)
		}
	case fieldID:
		return style.Foreground(lipgloss.Cyan)
	case fieldTokens:
		style = style.Foreground(lipgloss.Cyan)
		if s.TokensUsed >= 1_000_000 {
			return style.Foreground(lipgloss.BrightCyan).Bold(true)
		}
		if s.TokensUsed < 100_000 {
			return style.Faint(true)
		}
		return style
	case fieldCache:
		if s.CacheHitRate == nil {
			return style.Faint(true)
		}
		switch {
		case *s.CacheHitRate >= 0.90:
			return style.Foreground(lipgloss.Green)
		case *s.CacheHitRate >= 0.70:
			return style.Foreground(lipgloss.Yellow)
		default:
			return style.Foreground(lipgloss.Red)
		}
	default:
		return style
	}
}

func clip(value string, width int) string {
	value = clean(value)
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}
