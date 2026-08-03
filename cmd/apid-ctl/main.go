// Command apid-ctl lists local AI coding-agent sessions.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/abowloflrf/apid/session"
)

const usageText = `apid-ctl — list local Codex, Claude Code, pi, and OpenCode sessions.

Usage:
  apid-ctl [options]

Options:
  -n, --limit N       show at most N token-bearing sessions (default 20)
      --all           show all matching sessions
      --tool TOOL     codex | claude | pi | opencode | all (default all)
      --agent TOOL    alias for --tool
      --archived      show archived sessions instead of active sessions
      --cwd PATH      filter by cwd substring
      --source SRC    filter by source substring (Codex only)
      --since TIME    YYYY-MM-DD, ISO time, or relative 7d/12h/30m
      --sort KEY      updated | created (default updated)
      --json          emit script-compatible JSON
      --plain         print once instead of opening the interactive table
      --version       print version and exit

The interactive table is used when stdin and stdout are terminals. Use ↑/↓,
j/k, PgUp/PgDn, g/G to navigate and q, Esc, or Ctrl-C to quit.
`

type options struct {
	limit       int
	all         bool
	tool        string
	archived    bool
	cwd         string
	source      string
	since       string
	sort        string
	json        bool
	plain       bool
	showVersion bool
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "apid-ctl:", err)
		os.Exit(2)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, "apid-ctl", buildVersion())
		return nil
	}

	since, err := session.ParseSince(opts.since)
	if err != nil {
		return fmt.Errorf("invalid --since: %w", err)
	}
	archived := opts.archived
	query := session.Query{
		Archived: &archived,
		CWD:      opts.cwd,
		Source:   opts.source,
		Since:    since,
		Sort:     opts.sort,
		Limit:    opts.limit,
	}
	if opts.tool != "all" {
		query.Tools = []string{opts.tool}
	}

	loader := session.NewLoader()
	if opts.tool != "all" {
		loader = session.NewLoaderForTools(opts.tool)
	}
	var result session.Result
	if !opts.all && opts.limit <= 0 {
		query.Limit = 1
		result = loader.List(query, false)
		result.Sessions = nil
		result.Total = 0
	} else {
		if opts.all {
			query.Limit = 0
		}
		result = loader.Display(query)
	}

	if opts.json {
		return writeJSON(stdout, result.Sessions)
	}
	if opts.plain || len(result.Sessions) == 0 || !interactiveTerminal(stdin, stdout) {
		writeReport(stdout, result.Sources, result.Sessions)
		return nil
	}

	program := tea.NewProgram(
		newTUIModel(result.Sources, result.Sessions),
		tea.WithInput(stdin),
		tea.WithOutput(stdout),
	)
	_, err = program.Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("apid-ctl", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() { fmt.Fprint(output, usageText) }
	fs.IntVar(&opts.limit, "n", 20, "session limit")
	fs.IntVar(&opts.limit, "limit", 20, "session limit")
	fs.BoolVar(&opts.all, "all", false, "show all sessions")
	fs.StringVar(&opts.tool, "tool", "all", "agent tool")
	fs.StringVar(&opts.tool, "agent", "all", "agent tool")
	fs.BoolVar(&opts.archived, "archived", false, "show archived sessions")
	fs.StringVar(&opts.cwd, "cwd", "", "cwd substring")
	fs.StringVar(&opts.source, "source", "", "source substring")
	fs.StringVar(&opts.since, "since", "", "updated-since bound")
	fs.StringVar(&opts.sort, "sort", "updated", "sort key")
	fs.BoolVar(&opts.json, "json", false, "emit JSON")
	fs.BoolVar(&opts.plain, "plain", false, "disable the interactive table")
	fs.BoolVar(&opts.showVersion, "version", false, "print version")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch opts.tool {
	case "all", session.ToolCodex, session.ToolClaude, session.ToolPi, session.ToolOpenCode:
	default:
		return options{}, fmt.Errorf("invalid --tool %q", opts.tool)
	}
	if opts.sort != "updated" && opts.sort != "created" {
		return options{}, fmt.Errorf("invalid --sort %q", opts.sort)
	}
	return opts, nil
}

func interactiveTerminal(stdin io.Reader, stdout io.Writer) bool {
	in, inOK := stdin.(*os.File)
	out, outOK := stdout.(*os.File)
	return inOK && outOK && isCharacterDevice(in) && isCharacterDevice(out)
}

func isCharacterDevice(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
