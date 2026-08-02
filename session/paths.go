package session

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Path probing mirrors scripts/codex_sessions.py: env vars override, then
// platform defaults (XDG for opencode, $HOME for the rest).

func codexHome() string {
	return homeOr("CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex"))
}

func sqliteHome() string {
	if v := os.Getenv("CODEX_SQLITE_HOME"); v != "" {
		return v
	}
	return codexHome()
}

// findStateDB returns the newest state_*.sqlite (highest schema version).
func findStateDB() string {
	matches, _ := filepath.Glob(filepath.Join(sqliteHome(), "state_*.sqlite"))
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		return stateVersion(filepath.Base(matches[i])) > stateVersion(filepath.Base(matches[j]))
	})
	return matches[0]
}

func claudeHome() string {
	return homeOr("CLAUDE_HOME", filepath.Join(os.Getenv("HOME"), ".claude"))
}

func piHome() string {
	return homeOr("PI_CODING_AGENT_DIR", filepath.Join(os.Getenv("HOME"), ".pi", "agent"))
}

// opencodeDataHome follows the XDG base-directory rule per platform.
func opencodeDataHome() string {
	if v := os.Getenv("OPENCODE_DATA_HOME"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "opencode")
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(os.Getenv("HOME"), "AppData", "Roaming")
		}
		return filepath.Join(appdata, "opencode")
	default:
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg == "" {
			xdg = filepath.Join(os.Getenv("HOME"), ".local", "share")
		}
		return filepath.Join(xdg, "opencode")
	}
}

// findOpencodeDB returns the configured OPENCODE_DB (resolved against the data
// home when relative) or the most recently modified opencode*.db.
func findOpencodeDB() string {
	if v := os.Getenv("OPENCODE_DB"); v != "" {
		p := v
		if !filepath.IsAbs(p) {
			p = filepath.Join(opencodeDataHome(), p)
		}
		if fileExists(p) {
			return p
		}
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(opencodeDataHome(), "opencode*.db"))
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		fi1, err1 := os.Stat(matches[i])
		fi2, err2 := os.Stat(matches[j])
		if err1 != nil || err2 != nil {
			return false
		}
		return fi1.ModTime().After(fi2.ModTime())
	})
	return matches[0]
}
