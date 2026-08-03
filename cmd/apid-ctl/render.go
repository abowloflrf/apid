package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/abowloflrf/apid/session"
	"github.com/charmbracelet/x/term"
	"github.com/mattn/go-runewidth"
)

type outputSession struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	Source           string   `json:"source"`
	ModelProvider    string   `json:"model_provider"`
	CWD              string   `json:"cwd"`
	Model            string   `json:"model"`
	ReasoningEffort  string   `json:"reasoning_effort"`
	TokensUsed       int64    `json:"tokens_used"`
	Archived         bool     `json:"archived"`
	CliVersion       string   `json:"cli_version"`
	RolloutPath      string   `json:"rollout_path"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
	Tool             string   `json:"tool"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
}

func writeJSON(w io.Writer, sessions []session.Session) error {
	out := make([]outputSession, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, outputSession{
			ID:               s.ID,
			Title:            s.Title,
			CreatedAt:        formatTime(s.CreatedAt),
			UpdatedAt:        formatTime(s.UpdatedAt),
			Source:           s.Source,
			ModelProvider:    s.ModelProvider,
			CWD:              s.CWD,
			Model:            s.Model,
			ReasoningEffort:  s.ReasoningEffort,
			TokensUsed:       s.TokensUsed,
			Archived:         s.Archived,
			CliVersion:       s.CliVersion,
			RolloutPath:      s.RolloutPath,
			CacheHitRate:     s.CacheHitRate,
			Tool:             s.Tool,
			InputTokens:      s.InputTokens,
			OutputTokens:     s.OutputTokens,
			CacheReadTokens:  s.CacheReadTokens,
			CacheWriteTokens: s.CacheWriteTokens,
		})
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func writeReport(w io.Writer, sources []session.SourceInfo, sessions []session.Session) {
	colorize := plainColorEnabled(w)
	fmt.Fprintln(w, sourceLine(sources))
	fmt.Fprintln(w)
	if len(sessions) == 0 {
		fmt.Fprintln(w, "没有找到匹配的会话。")
		fmt.Fprintln(w, summaryLine(nil))
		return
	}

	writePlainTable(w, sessions, plainWidth(w), colorize)
	count := fmt.Sprintf("共 %d 条会话", len(sessions))
	if colorize {
		count = lipgloss.NewStyle().Faint(true).Render(count)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, count)
	fmt.Fprintln(w, summaryLine(sessions))
}

func writePlainTable(w io.Writer, sessions []session.Session, width int, colorize bool) {
	layout := layoutForWidth(max(20, width))
	writeRow := func(cells []string, styleFor func(int) lipgloss.Style) {
		for i, column := range layout.columns {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			cell := plainCell(cells[i], column.Width)
			if colorize {
				cell = styleFor(i).Render(cell)
			}
			fmt.Fprint(w, cell)
		}
		fmt.Fprintln(w)
	}

	headings := make([]string, len(layout.columns))
	for i, column := range layout.columns {
		headings[i] = column.Title
	}
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan)
	writeRow(headings, func(int) lipgloss.Style { return headerStyle })
	for i, row := range rowsForLayout(sessions, layout) {
		s := sessions[i]
		writeRow(row, func(column int) lipgloss.Style {
			return sessionCellStyle(s, layout.fields[column])
		})
	}
}

func plainColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if force := os.Getenv("FORCE_COLOR"); force != "" && force != "0" {
		return true
	}
	file, ok := w.(*os.File)
	return ok && isCharacterDevice(file)
}

func plainWidth(w io.Writer) int {
	if file, ok := w.(*os.File); ok {
		if width, _, err := term.GetSize(file.Fd()); err == nil && width > 0 {
			return width
		}
	}
	if width, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && width > 0 {
		return width
	}
	return 128
}

func plainCell(value string, width int) string {
	value = runewidth.Truncate(clean(value), width, "…")
	return value + strings.Repeat(" ", max(0, width-runewidth.StringWidth(value)))
}

func sourceLine(sources []session.SourceInfo) string {
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		parts = append(parts, source.Label+": "+source.Desc)
	}
	return "数据源: " + strings.Join(parts, ", ")
}

func summaryLine(sessions []session.Session) string {
	sum := session.Summarize(sessions)
	return fmt.Sprintf(
		"Token 总计（当前列表）：总计 %s (%s)，输入 %s (%s)，输出 %s (%s)，缓存命中 %s",
		formatTokens(sum.TotalTokens), formatInteger(sum.TotalTokens),
		formatTokens(sum.InputTokens), formatInteger(sum.InputTokens),
		formatTokens(sum.OutputTokens), formatInteger(sum.OutputTokens),
		formatCache(sum.CachePct),
	)
}

func formatTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

func formatShortTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format("01-02 15:04")
}

func formatTokens(value int64) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%.0fK", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}

func formatInteger(value int64) string {
	raw := fmt.Sprintf("%d", value)
	for i := len(raw) - 3; i > 0; i -= 3 {
		raw = raw[:i] + "," + raw[i:]
	}
	return raw
}

func formatCache(rate *float64) string {
	if rate == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *rate*100)
}

func clean(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func toolLabel(tool string) string {
	switch tool {
	case session.ToolClaude:
		return "CC"
	case session.ToolPi:
		return "PI"
	case session.ToolOpenCode:
		return "OC"
	default:
		return "CX"
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func shortHome(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return strings.ReplaceAll(path, home, "~")
	}
	return path
}
