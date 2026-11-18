package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// DropRecord represents a single recorded drop entry from the CSV
type DropRecord struct {
	Timestamp   string `json:"timestamp"`
	Location    string `json:"location"`
	MatchedItem string `json:"matched_item"`
	RawLine     string `json:"raw_line"`
}

// VelocityStats holds total counts and hourly drop rates
type VelocityStats struct {
	Total      int     `json:"total"`
	PerHour    float64 `json:"per_hour"`
	Percentage float64 `json:"percentage"`
}

// LocationStat holds drop count and velocity for a specific zone
type LocationStat struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	PerHour float64 `json:"per_hour"`
}

// MetricsSummary encapsulates all calculated dashboard analytics
type MetricsSummary struct {
	TotalDrops    VelocityStats  `json:"total_drops"`
	RunesFound    VelocityStats  `json:"runes_found"`
	HighRunes     VelocityStats  `json:"high_runes"`
	WSSFound      VelocityStats  `json:"wss_found"`
	KeysFound     VelocityStats  `json:"keys_found"`
	PuzzleFound   VelocityStats  `json:"puzzle_found"`
	MirrorsVials  VelocityStats  `json:"mirrors_vials"`
	TopLocations  []LocationStat `json:"top_locations"`
	CategoryBreak map[string]int `json:"category_break"`
	RuneCounts    map[string]int `json:"rune_counts"`
}

// AppState manages the Python child process and live console output
type AppState struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	logs    []string
	logSeq  int
}

var state = &AppState{
	logs:   []string{"[Manager] Ready. Click 'Start Logger' to begin watching Diablo II."},
	logSeq: 1,
}

func (s *AppState) addLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, line)
	s.logSeq++
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
}

// getExeDir returns the absolute directory where D2DropManager.exe or main.go runs
func getExeDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "d2_screen_logger.py")); err == nil {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "d2_screen_logger.py")); err == nil {
			return cwd
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		tessDir := filepath.Join(home, "Desktop", "tesseract")
		if _, err := os.Stat(tessDir); err == nil {
			return tessDir
		}
		return tessDir
	}
	return "."
}

func getCSVPath() string {
	return filepath.Join(getExeDir(), "my_d2_drops.csv")
}

func readDropsCSV() []DropRecord {
	path := getCSVPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []DropRecord{}
	}

	file, err := os.Open(path)
	if err != nil {
		return []DropRecord{}
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1

	rows, err := reader.ReadAll()
	if err != nil || len(rows) == 0 {
		return []DropRecord{}
	}

	var records []DropRecord
	startIndex := 0
	if len(rows) > 0 && len(rows[0]) > 0 {
		h0 := strings.ToLower(strings.TrimSpace(rows[0][0]))
		if strings.Contains(h0, "timestamp") || strings.Contains(h0, "date") {
			startIndex = 1
		}
	}

	for i := len(rows) - 1; i >= startIndex; i-- {
		row := rows[i]
		if len(row) >= 4 {
			records = append(records, DropRecord{
				Timestamp:   strings.TrimSpace(row[0]),
				Location:    strings.TrimSpace(row[1]),
				MatchedItem: strings.TrimSpace(row[2]),
				RawLine:     strings.TrimSpace(row[3]),
			})
		}
	}
	return records
}

// Enforce explicit capitalization for Lo vs Io so lowercase "lo" (misread Io) never counts as a High Rune
var highRuneRegex = regexp.MustCompile(`(?i)\b(pul|um|mal|ist|gul|vex|ohm|sur|ber|jah|cham|zod)\s+rune\b|\b(?:Lo|LO)\s+(?:Rune|RUNE|rune)\b`)
var runeRegex = regexp.MustCompile(`(?i)\b(el|eld|tir|nef|eth|ith|tal|ral|ort|thul|amn|sol|shael|dol|hel|lum|ko|fal|lem|pul|um|mal|ist|gul|vex|ohm|sur|ber|jah|cham|zod)\s+rune\b|\b(?:Io|IO|Lo|LO)\s+(?:Rune|RUNE|rune)\b`)
var wssRegex = regexp.MustCompile(`(?i)worldstone\s+shard`)
var keyRegex = regexp.MustCompile(`(?i)key\s+of\s+(terror|hate|destruction)`)
var puzzleRegex = regexp.MustCompile(`(?i)puzzle\s*box|puzzle\s*piece`)
var mirrorVialRegex = regexp.MustCompile(`(?i)lilith'?s\s+mirror|vial\s+of\s+lightsong`)

func extractRuneName(text string) string {
	m := runeRegex.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	fields := strings.Fields(m[0])
	if len(fields) == 0 {
		return ""
	}
	r := strings.ToLower(fields[0])
	if len(r) == 0 {
		return ""
	}
	return strings.ToUpper(r[:1]) + r[1:]
}

func sortMapToPairs(m map[string]int, limit int) []LocationStat {
	var pairs []LocationStat
	for k, v := range m {
		pairs = append(pairs, LocationStat{Name: k, Count: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Count == pairs[j].Count {
			return pairs[i].Name < pairs[j].Name
		}
		return pairs[i].Count > pairs[j].Count
	})
	if limit > 0 && len(pairs) > limit {
		pairs = pairs[:limit]
	}
	return pairs
}

func calculateMetrics(drops []DropRecord) MetricsSummary {
	total := len(drops)
	var runes, highRunes, wss, keys, puzzle, mirrorsVials int
	locMap := make(map[string]int)
	runeCounts := make(map[string]int)

	firstTime := time.Now()
	lastTime := time.Now()

	if total > 0 {
		if t, err := time.Parse("2006-01-02 15:04:05", drops[len(drops)-1].Timestamp); err == nil {
			firstTime = t
		}
		if t, err := time.Parse("2006-01-02 15:04:05", drops[0].Timestamp); err == nil {
			lastTime = t
		}
	}

	hours := lastTime.Sub(firstTime).Hours()
	if hours < 0.25 {
		hours = 0.25
	}

	for _, d := range drops {
		// Check MatchedItem primarily so OCR noise in RawLine doesn't trigger a second match
		targetText := strings.TrimSpace(d.MatchedItem)
		if targetText == "" {
			targetText = d.RawLine
		}

		loc := d.Location
		if loc == "" {
			loc = "Unknown Area"
		}
		locMap[loc]++

		if rName := extractRuneName(targetText); rName != "" {
			runes++
			runeCounts[rName]++
		}
		if highRuneRegex.MatchString(targetText) {
			highRunes++
		}
		if wssRegex.MatchString(targetText) {
			wss++
		}
		if keyRegex.MatchString(targetText) {
			keys++
		}
		if puzzleRegex.MatchString(targetText) {
			puzzle++
		}
		if mirrorVialRegex.MatchString(targetText) {
			mirrorsVials++
		}
	}

	calcStat := func(count int) VelocityStats {
		pct := 0.0
		if total > 0 {
			pct = (float64(count) / float64(total)) * 100
		}
		return VelocityStats{
			Total:      count,
			PerHour:    float64(count) / hours,
			Percentage: pct,
		}
	}

	rawTop := sortMapToPairs(locMap, 10)
	topLocs := make([]LocationStat, 0, len(rawTop))
	for _, l := range rawTop {
		topLocs = append(topLocs, LocationStat{
			Name:    l.Name,
			Count:   l.Count,
			PerHour: float64(l.Count) / hours,
		})
	}

	return MetricsSummary{
		TotalDrops:   calcStat(total),
		RunesFound:   calcStat(runes),
		HighRunes:    calcStat(highRunes),
		WSSFound:     calcStat(wss),
		KeysFound:    calcStat(keys),
		PuzzleFound:  calcStat(puzzle),
		MirrorsVials: calcStat(mirrorsVials),
		TopLocations: topLocs,
		CategoryBreak: map[string]int{
			"Runes":             runes,
			"High Runes":        highRunes,
			"WSS":               wss,
			"Keys":              keys,
			"Puzzle Box/Pieces": puzzle,
			"Mirrors / Vials":   mirrorsVials,
		},
		RuneCounts: runeCounts,
	}
}

func findPythonCmd() (string, error) {
	candidates := []string{"python", "py", "python3"}
	for _, cand := range candidates {
		if path, err := exec.LookPath(cand); err == nil {
			return path, nil
		}
	}
	if runtime.GOOS == "windows" {
		winCandidates := []string{
			`C:\Program Files\Python313\python.exe`,
			`C:\Program Files\Python312\python.exe`,
			`C:\Program Files\Python311\python.exe`,
			`C:\Program Files\Python310\python.exe`,
			`C:\Program Files\Python39\python.exe`,
		}
		for _, wc := range winCandidates {
			if _, err := os.Stat(wc); err == nil {
				return wc, nil
			}
		}
	}
	return "", fmt.Errorf("no python executable found in system PATH")
}

func (s *AppState) streamOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.addLog(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		s.addLog(fmt.Sprintf("[Manager] Pipe stream warning: %v", err))
	}
}

func (s *AppState) startPython() error {
	s.mu.Lock()
	if s.running && s.cmd != nil && s.cmd.Process != nil {
		s.mu.Unlock()
		return fmt.Errorf("logger is already running")
	}
	s.mu.Unlock()

	pyBin, err := findPythonCmd()
	if err != nil {
		return err
	}

	workDir := getExeDir()
	scriptPath := filepath.Join(workDir, "d2_screen_logger.py")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return fmt.Errorf("d2_screen_logger.py not found in %s", workDir)
	}

	var cmd *exec.Cmd
	if strings.EqualFold(filepath.Base(pyBin), "py.exe") || filepath.Base(pyBin) == "py" {
		cmd = exec.Command(pyBin, "-3", "d2_screen_logger.py")
	} else {
		cmd = exec.Command(pyBin, "d2_screen_logger.py")
	}
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start python process: %v", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.running = true
	currentCmd := cmd
	s.mu.Unlock()

	s.addLog(fmt.Sprintf("[Manager] Started d2_screen_logger.py using %s in %s...", filepath.Base(pyBin), workDir))

	go s.streamOutput(stdout)
	go s.streamOutput(stderr)

	go func() {
		err := currentCmd.Wait()
		s.mu.Lock()
		wasCurrent := (s.cmd == currentCmd)
		if wasCurrent {
			s.running = false
			s.cmd = nil
		}
		s.mu.Unlock()

		if wasCurrent {
			if err != nil {
				s.addLog(fmt.Sprintf("[Manager] Logger stopped (%v).", err))
			} else {
				s.addLog("[Manager] Logger stopped cleanly.")
			}
		}
	}()

	return nil
}

func (s *AppState) stopPython() {
	s.mu.Lock()
	cmdToStop := s.cmd
	s.running = false
	s.cmd = nil
	s.mu.Unlock()

	if cmdToStop != nil && cmdToStop.Process != nil {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", cmdToStop.Process.Pid)).Run()
		} else {
			_ = cmdToStop.Process.Kill()
		}
		s.addLog("[Manager] Logger stopped by user.")
	}
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = exec.Command("xdg-open", url).Start()
	}
	if err != nil {
		fmt.Printf("Please open your browser to: %s\n", url)
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>D2DropManager Dashboard</title>
<style>
  :root, [data-theme="neon"] {
    --bg-dark: #030704;
    --card-bg: #08120a;
    --border: #143d1d;
    --accent: #39ff14;
    --accent-hover: #2ce60d;
    --text-main: #e2fbe6;
    --text-muted: #68a373;
    --success: #39ff14;
    --btn-start-text: #020803;
    --danger: #ff3b30;
    --console-bg: #010402;
    --console-text: #39ff14;
    --rune-color: #ff8c00;
  }
  [data-theme="sanctuary"] {
    --bg-dark: #0f172a;
    --card-bg: #1e293b;
    --border: #334155;
    --accent: #38bdf8;
    --accent-hover: #0ea5e9;
    --text-main: #f8fafc;
    --text-muted: #94a3b8;
    --success: #22c55e;
    --btn-start-text: #ffffff;
    --danger: #ef4444;
    --console-bg: #090d16;
    --console-text: #38bdf8;
    --rune-color: #fb923c;
  }
  [data-theme="hellfire"] {
    --bg-dark: #120606;
    --card-bg: #1f0a0a;
    --border: #4a1515;
    --accent: #f59e0b;
    --accent-hover: #d97706;
    --text-main: #fef2f2;
    --text-muted: #fca5a5;
    --success: #10b981;
    --btn-start-text: #ffffff;
    --danger: #ef4444;
    --console-bg: #090202;
    --console-text: #fbbf24;
    --rune-color: #ff6b00;
  }
  [data-theme="tyrael"] {
    --bg-dark: #0b0e17;
    --card-bg: #13192b;
    --border: #2a3659;
    --accent: #eab308;
    --accent-hover: #ca8a04;
    --text-main: #f8fafc;
    --text-muted: #93c5fd;
    --success: #22c55e;
    --btn-start-text: #ffffff;
    --danger: #ef4444;
    --console-bg: #06080f;
    --console-text: #fde047;
    --rune-color: #ff8c00;
  }
  [data-theme="horadric"] {
    --bg-dark: #130b1c;
    --card-bg: #1e122b;
    --border: #3b2354;
    --accent: #c084fc;
    --accent-hover: #a855f7;
    --text-main: #faf5ff;
    --text-muted: #d8b4fe;
    --success: #34d399;
    --btn-start-text: #061b12;
    --danger: #f43f5e;
    --console-bg: #09040f;
    --console-text: #e879f9;
    --rune-color: #ff941a;
  }
  [data-theme="bloodmoor"] {
    --bg-dark: #0d0507;
    --card-bg: #1a0a0e;
    --border: #4d1622;
    --accent: #e11d48;
    --accent-hover: #be123c;
    --text-main: #f5ebe0;
    --text-muted: #c996a0;
    --success: #22c55e;
    --btn-start-text: #ffffff;
    --danger: #f43f5e;
    --console-bg: #070203;
    --console-text: #fb7185;
    --rune-color: #ff8c00;
  }
  [data-theme="mephisto"] {
    --bg-dark: #050d0a;
    --card-bg: #0b1a14;
    --border: #174734;
    --accent: #10b981;
    --accent-hover: #059669;
    --text-main: #ecfdf5;
    --text-muted: #6ee7b7;
    --success: #34d399;
    --btn-start-text: #02140d;
    --danger: #ef4444;
    --console-bg: #020805;
    --console-text: #34d399;
    --rune-color: #ff941a;
  }
  [data-theme="worldstone"] {
    --bg-dark: #070c14;
    --card-bg: #0e1726;
    --border: #233859;
    --accent: #7dd3fc;
    --accent-hover: #38bdf8;
    --text-main: #f0f9ff;
    --text-muted: #93c5fd;
    --success: #38bdf8;
    --btn-start-text: #04101c;
    --danger: #ef4444;
    --console-bg: #03060a;
    --console-text: #bae6fd;
    --rune-color: #fbbf24;
  }
  [data-theme="chaos"] {
    --bg-dark: #0f0704;
    --card-bg: #1c0d07;
    --border: #54220e;
    --accent: #f97316;
    --accent-hover: #ea580c;
    --text-main: #fff7ed;
    --text-muted: #fdba74;
    --success: #22c55e;
    --btn-start-text: #ffffff;
    --danger: #dc2626;
    --console-bg: #080301;
    --console-text: #fb923c;
    --rune-color: #facc15;
  }
  [data-theme="tristram"] {
    --bg-dark: #12100d;
    --card-bg: #1f1b16;
    --border: #473d31;
    --accent: #d97706;
    --accent-hover: #b45309;
    --text-main: #f5f0e6;
    --text-muted: #b8ab96;
    --success: #65a30d;
    --btn-start-text: #ffffff;
    --danger: #dc2626;
    --console-bg: #0a0807;
    --console-text: #fbbf24;
    --rune-color: #ff8c00;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  body { background-color: var(--bg-dark); color: var(--text-main); min-height: 100vh; display: flex; flex-direction: column; transition: background-color 0.2s, color 0.2s; }
  header { background: var(--card-bg); border-bottom: 1px solid var(--border); padding: 16px 24px; display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 12px; }
  h1 { font-size: 1.25rem; font-weight: 600; display: flex; align-items: center; gap: 10px; color: var(--accent); }
  .badge { font-size: 0.75rem; padding: 4px 10px; border-radius: 999px; font-weight: 500; text-transform: uppercase; }
  .badge.running { background: rgba(57, 255, 20, 0.15); color: var(--success); border: 1px solid var(--success); }
  .badge.stopped { background: rgba(239, 68, 68, 0.15); color: var(--danger); border: 1px solid rgba(239, 68, 68, 0.4); }
  .controls { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
  select.theme-select { background: var(--bg-dark); color: var(--text-main); border: 1px solid var(--border); padding: 7px 12px; border-radius: 6px; font-size: 0.875rem; font-weight: 500; cursor: pointer; outline: none; }
  select.theme-select:focus { border-color: var(--accent); }
  button { padding: 8px 16px; border-radius: 6px; font-weight: 600; font-size: 0.875rem; cursor: pointer; border: none; transition: background 0.2s, opacity 0.2s, border-color 0.2s; }
  button:disabled { opacity: 0.45; cursor: not-allowed; }
  .btn-start { background: var(--success); color: var(--btn-start-text); }
  .btn-start:hover:not(:disabled) { filter: brightness(0.92); }
  .btn-stop { background: var(--danger); color: white; }
  .btn-stop:hover:not(:disabled) { filter: brightness(0.92); }
  .btn-secondary { background: var(--border); color: var(--text-main); }
  .btn-secondary:hover { filter: brightness(1.15); }
  .btn-rune-toggle { background: var(--bg-dark); color: var(--rune-color); border: 1px solid var(--rune-color); }
  .btn-rune-toggle:hover, .btn-rune-toggle.active { background: var(--rune-color); color: #050505; }
  main { flex: 1; padding: 24px; max-width: 1480px; width: 100%; margin: 0 auto; display: flex; flex-direction: column; gap: 24px; }
  .metrics-grid { display: grid; grid-template-columns: repeat(7, 1fr); gap: 14px; }
  @media(max-width: 1300px) { .metrics-grid { grid-template-columns: repeat(4, 1fr); } }
  @media(max-width: 900px) { .metrics-grid { grid-template-columns: repeat(2, 1fr); } }
  @media(max-width: 600px) { .metrics-grid { grid-template-columns: 1fr; } }
  .metric-card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 14px; display: flex; flex-direction: column; gap: 8px; }
  .metric-title { font-size: 0.775rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; }
  .metric-value { font-size: 1.65rem; font-weight: 700; color: var(--text-main); }
  .metric-sub { font-size: 0.8rem; color: var(--accent); display: flex; justify-content: space-between; }
  .content-split { display: grid; grid-template-columns: 1fr 1fr; gap: 24px; }
  @media(max-width: 900px) { .content-split { grid-template-columns: 1fr; } }
  .panel { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; display: flex; flex-direction: column; height: 420px; overflow: hidden; }
  .panel-header { padding: 12px 16px; border-bottom: 1px solid var(--border); font-weight: 600; font-size: 0.9375rem; display: flex; justify-content: space-between; align-items: center; }
  .console-box { flex: 1; background: var(--console-bg); color: var(--console-text); font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 0.8125rem; padding: 16px; overflow-y: auto; white-space: pre-wrap; word-break: break-all; line-height: 1.4; }
  .table-container { flex: 1; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; text-align: left; font-size: 0.875rem; }
  th, td { padding: 10px 16px; border-bottom: 1px solid var(--border); }
  th { background: rgba(255,255,255,0.03); color: var(--text-muted); font-weight: 500; position: sticky; top: 0; backdrop-filter: blur(4px); }
  tr:hover td { background: rgba(255,255,255,0.03); }
  .rune-item { color: var(--rune-color); font-weight: 700; }

  /* Draggable Pop-Out Rune Map */
  .rune-popover {
    position: fixed;
    top: 84px;
    right: 28px;
    width: 468px;
    max-width: calc(100vw - 32px);
    background: var(--card-bg);
    border: 2px solid var(--accent);
    border-radius: 10px;
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.85);
    z-index: 9999;
    display: none;
    flex-direction: column;
    overflow: hidden;
  }
  .rune-popover.open { display: flex; }
  .rune-popover-header {
    padding: 10px 14px;
    background: var(--bg-dark);
    border-bottom: 1px solid var(--border);
    display: flex;
    justify-content: space-between;
    align-items: center;
    cursor: move;
    user-select: none;
  }
  .rune-popover-title {
    font-size: 0.875rem;
    font-weight: 700;
    color: var(--rune-color);
    letter-spacing: 0.04em;
    text-transform: uppercase;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .rune-popover-close {
    background: transparent;
    color: var(--text-muted);
    padding: 2px 8px;
    font-size: 1rem;
    border-radius: 4px;
  }
  .rune-popover-close:hover { background: var(--border); color: var(--text-main); }
  .rune-stash-body {
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    background: radial-gradient(circle at center, rgba(255,255,255,0.02), rgba(0,0,0,0.45));
  }
  .rune-row {
    display: grid;
    grid-template-columns: repeat(9, 1fr);
    gap: 5px;
  }
  .rune-row.row-bottom {
    display: flex;
    justify-content: center;
    gap: 5px;
  }
  .rune-row.row-bottom .rune-slot {
    width: calc((100% - 40px) / 9);
  }
  .rune-slot {
    background: var(--bg-dark);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 5px 2px 4px 2px;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: space-between;
    opacity: 0.36;
    transition: opacity 0.2s, transform 0.15s, border-color 0.2s, box-shadow 0.2s;
  }
  .rune-slot.found {
    opacity: 1;
    border-color: var(--rune-color);
    box-shadow: inset 0 0 10px rgba(255, 140, 0, 0.18);
 }
  .rune-slot.high-rune.found {
    border-color: var(--accent);
    box-shadow: 0 0 10px rgba(255, 140, 0, 0.35), inset 0 0 12px rgba(255, 215, 0, 0.22);
  }
  .rune-slot.wss-slot.found {
    border-color: #c084fc;
    box-shadow: 0 0 10px rgba(192, 132, 252, 0.4), inset 0 0 12px rgba(168, 85, 247, 0.25);
  }
  .rune-slot.wss-slot.found .rune-name {
    color: #d8b4fe;
  }
  .rune-stone {
    width: 28px;
    height: 28px;
    background: linear-gradient(145deg, #52565e, #2b2e33);
    border: 1px solid #767b85;
    clip-path: polygon(22% 4%, 78% 4%, 96% 88%, 86% 96%, 14% 96%, 4% 88%);
    display: flex;
    align-items: center;
    justify-content: center;
    font-family: Georgia, "Times New Roman", serif;
    font-weight: 700;
    font-size: 0.72rem;
    color: #16181c;
    text-shadow: 0 1px 0 rgba(255,255,255,0.28);
    margin-bottom: 3px;
  }
  .rune-slot.found .rune-stone {
    background: linear-gradient(145deg, #696e78, #373b42);
    color: var(--rune-color);
    text-shadow: 0 0 5px rgba(255, 140, 0, 0.75);
  }
  .rune-slot.high-rune.found .rune-stone {
    color: #ffd700;
    text-shadow: 0 0 6px rgba(255, 215, 0, 0.9);
  }
  .rune-name {
    font-size: 0.65rem;
    font-weight: 700;
    color: var(--text-muted);
    letter-spacing: 0.02em;
    line-height: 1.1;
  }
  .rune-slot.found .rune-name { color: var(--rune-color); }
  .rune-count {
    font-size: 0.75rem;
    font-weight: 800;
    color: var(--text-main);
    margin-top: 2px;
    line-height: 1.1;
  }
  .rune-footer {
    padding: 7px 14px;
    background: var(--bg-dark);
    border-top: 1px solid var(--border);
    font-size: 0.75rem;
    color: var(--text-muted);
    display: flex;
    justify-content: space-between;
  }
</style>
</head>
<body>
<header>
  <h1>
    <svg width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24"><path d="M9 3v2m6-2v2M9 19v2m6-2v2M5 9H3m2 6H3m18-6h-2m2 6h-2M7 19h10a2 2 0 002-2V7a2 2 0 00-2-2H7a2 2 0 00-2 2v10a2 2 0 002 2z"/></svg>
    D2DropManager
  </h1>
  <div style="display:flex; align-items:center; gap: 12px; flex-wrap: wrap;">
    <select id="themeSelect" class="theme-select" onchange="setTheme(this.value)" title="Select UI Theme">
      <option value="neon">Theme: Neon Matrix (Default)</option>
      <option value="sanctuary">Theme: Sanctuary Slate</option>
      <option value="hellfire">Theme: Hellfire Crimson</option>
      <option value="tyrael">Theme: Tyrael Gold</option>
      <option value="horadric">Theme: Horadric Arcane</option>
      <option value="bloodmoor">Theme: Countess Blood Moor</option>
      <option value="mephisto">Theme: Mephisto's Durance</option>
      <option value="worldstone">Theme: Baal's Worldstone</option>
      <option value="chaos">Theme: Chaos Sanctuary</option>
      <option value="tristram">Theme: Old Tristram</option>
    </select>
    <div id="statusBadge" class="badge stopped">Stopped</div>
    <div class="controls">
      <button id="btnStart" class="btn-start" onclick="api('/api/start')">▶ Start Logger</button>
      <button id="btnStop" class="btn-stop" onclick="api('/api/stop')" disabled>■ Stop</button>
      <button id="btnRuneMap" class="btn-rune-toggle" onclick="toggleRuneMap()">🌎 Rune Map</button>
      <button class="btn-secondary" onclick="api('/api/open-folder')">📁 Open Folder</button>
      <button class="btn-secondary" onclick="api('/api/quit')">✕ Exit</button>
    </div>
  </div>
</header>

<!-- Draggable Floating Rune Stash Map -->
<div id="runePopover" class="rune-popover">
  <div id="runePopoverHeader" class="rune-popover-header">
    <div class="rune-popover-title">🛒 PD2 Rune Stash Tracker (Drag to Move)</div>
    <button class="rune-popover-close" onclick="toggleRuneMap(false)" title="Close Rune Map">✕</button>
  </div>
  <div id="runeStashBody" class="rune-stash-body"></div>
  <div class="rune-footer">
    <span id="runeUniqueCount">0 / 33 Runes | WSS: 0</span>
    <span id="runeTotalPop">Total Runes: 0</span>
  </div>
</div>

<main>
  <div class="metrics-grid">
    <div class="metric-card">
      <div class="metric-title">Total Drops</div>
      <div id="mTotal" class="metric-value">0</div>
      <div class="metric-sub"><span id="mTotalHr">0.0 / hr</span><span>100%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Runes Found</div>
      <div id="mRunes" class="metric-value">0</div>
      <div class="metric-sub"><span id="mRunesHr">0.0 / hr</span><span id="mRunesPct">0.000000000%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Mid-High Runes (Pul+)</div>
      <div id="mHighRunes" class="metric-value">0</div>
      <div class="metric-sub"><span id="mHighRunesHr">0.0 / hr</span><span id="mHighRunesPct">0.000000000%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Worldstone Shards</div>
      <div id="mWss" class="metric-value">0</div>
      <div class="metric-sub"><span id="mWssHr">0.0 / hr</span><span id="mWssPct">0.000000000%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Keys Found</div>
      <div id="mKeys" class="metric-value">0</div>
      <div class="metric-sub"><span id="mKeysHr">0.0 / hr</span><span id="mKeysPct">0.000000000%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Puzzle Box / Pieces</div>
      <div id="mPuzzle" class="metric-value">0</div>
      <div class="metric-sub"><span id="mPuzzleHr">0.0 / hr</span><span id="mPuzzlePct">0.000000000%</span></div>
    </div>
    <div class="metric-card">
      <div class="metric-title">Mirrors / Vials</div>
      <div id="mMirrors" class="metric-value">0</div>
      <div class="metric-sub"><span id="mMirrorsHr">0.0 / hr</span><span id="mMirrorsPct">0.000000000%</span></div>
    </div>
  </div>

  <div class="content-split">
    <div class="panel">
      <div class="panel-header"><span>Top Drop Locations (Top 10)</span></div>
      <div class="table-container">
        <table>
          <thead><tr><th>Location</th><th>Drops</th><th>Rate</th></tr></thead>
          <tbody id="locationsTable"><tr><td colspan="3" style="text-align:center; color:var(--text-muted)">No drop locations recorded yet</td></tr></tbody>
        </table>
      </div>
    </div>
    <div class="panel">
      <div class="panel-header"><span>Live Scanner Feed</span></div>
      <div id="consoleBox" class="console-box">Connecting...</div>
    </div>
  </div>

  <div class="panel" style="height: 380px;">
    <div class="panel-header"><span>Recent Drops History</span></div>
    <div class="table-container">
      <table>
        <thead><tr><th>Timestamp</th><th>Location</th><th>Matched Item</th><th>Raw OCR Line</th></tr></thead>
        <tbody id="dropsTable"><tr><td colspan="4" style="text-align:center; color:var(--text-muted)">No drops recorded yet. Start your game and begin farming!</td></tr></tbody>
      </table>
    </div>
  </div>
</main>
<script>
  let lastLogSeq = -1;

  const RUNE_ROWS = [
    [
      { name: 'El', num: 1, glyph: 'ᛚ' }, { name: 'Eld', num: 2, glyph: 'ᛉ' }, { name: 'Tir', num: 3, glyph: 'ᛏ' },
      { name: 'Nef', num: 4, glyph: 'ᚺ' }, { name: 'Eth', num: 5, glyph: 'ᛊ' }, { name: 'Ith', num: 6, glyph: 'ᛇ' },
      { name: 'Tal', num: 7, glyph: 'ᛟ' }, { name: 'Ral', num: 8, glyph: 'ᚱ' }, { name: 'Ort', num: 9, glyph: 'ᚹ' }
    ],
    [
      { name: 'Thul', num: 10, glyph: 'ᚦ' }, { name: 'Amn', num: 11, glyph: 'ᛃ' }, { name: 'Sol', num: 12, glyph: 'ᛈ' },
      { name: 'Shael', num: 13, glyph: 'ᛒ' }, { name: 'Dol', num: 14, glyph: 'ᛞ' }, { name: 'Hel', num: 15, glyph: 'ᚻ' },
      { name: 'Io', num: 16, glyph: 'ᛁ' }, { name: 'Lum', num: 17, glyph: 'ᛚ' }, { name: 'Ko', num: 18, glyph: 'ᚲ' }
    ],
    [
      { name: 'Fal', num: 19, glyph: 'ᚠ' }, { name: 'Lem', num: 20, glyph: 'ᛗ' }, { name: 'Pul', num: 21, glyph: 'ᛈ', hr: true },
      { name: 'Um', num: 22, glyph: 'ᚢ', hr: true }, { name: 'Mal', num: 23, glyph: 'ᛗ', hr: true }, { name: 'Ist', num: 24, glyph: 'ᛇ', hr: true },
      { name: 'Gul', num: 25, glyph: 'ᚷ', hr: true }, { name: 'Vex', num: 26, glyph: 'ᚹ', hr: true }, { name: 'Ohm', num: 27, glyph: 'ᛟ', hr: true }
    ],
    [
      { name: 'Lo', num: 28, glyph: 'ᛚ', hr: true }, { name: 'Sur', num: 29, glyph: 'ᛊ', hr: true }, { name: 'Ber', num: 30, glyph: 'ᛒ', hr: true },
      { name: 'Jah', num: 31, glyph: 'ᛃ', hr: true }, { name: 'Cham', num: 32, glyph: 'ᚲ', hr: true }, { name: 'Zod', num: 33, glyph: 'ᛉ', hr: true },
      { name: 'WSS', num: 34, isWss: true }
    ]
  ];

  const WSS_FALLBACK_SVG = '<svg width="24" height="24" viewBox="0 0 24 24" fill="none">' +
    '<polygon points="12,1 19,7 17,22 7,22 5,7" fill="#7e22ce" stroke="#e9d5ff" stroke-width="1.4"/>' +
    '<polygon points="12,1 16,8 12,22 8,8" fill="#c084fc" opacity="0.85"/>' +
    '</svg>';

  function renderRuneMap(runeCounts, wssTotal) {
    const body = document.getElementById('runeStashBody');
    if (!body) return;
    const counts = runeCounts || {};
    const wssCount = wssTotal || 0;
    let uniqueFound = 0;
    let totalRunes = 0;

    body.innerHTML = RUNE_ROWS.map((row, idx) => {
      const rowClass = idx === 3 ? 'rune-row row-bottom' : 'rune-row';
      const slots = row.map(r => {
        if (r.isWss) {
          const cls = 'rune-slot wss-slot' + (wssCount > 0 ? ' found' : '');
          const iconHtml = '<div style="width:28px;height:28px;display:flex;align-items:center;justify-content:center;margin-bottom:3px;">' +
            '<img src="/assets/wss.png" alt="WSS" style="width:26px;height:26px;object-fit:contain;" onerror="this.style.display=\'none\';this.nextElementSibling.style.display=\'flex\';">' +
            '<div style="display:none;align-items:center;justify-content:center;">' + WSS_FALLBACK_SVG + '</div>' +
          '</div>';
          return '<div class="' + cls + '" title="Worldstone Shard (' + wssCount + ' found)">' +
            iconHtml +
            '<div class="rune-name">WSS</div>' +
            '<div class="rune-count">' + wssCount + '</div>' +
          '</div>';
        }
        const c = counts[r.name] || 0;
        if (c > 0) uniqueFound++;
        totalRunes += c;
        const cls = 'rune-slot' + (c > 0 ? ' found' : '') + (r.hr ? ' high-rune' : '');
        return '<div class="' + cls + '" title="#' + r.num + ' ' + r.name + ' Rune (' + c + ' found)">' +
          '<div class="rune-stone">' + r.glyph + '</div>' +
          '<div class="rune-name">' + r.name + '</div>' +
          '<div class="rune-count">' + c + '</div>' +
        '</div>';
      }).join('');
      return '<div class="' + rowClass + '">' + slots + '</div>';
    }).join('');

    const uEl = document.getElementById('runeUniqueCount');
    const tEl = document.getElementById('runeTotalPop');
    if (uEl) uEl.textContent = uniqueFound + ' / 33 Runes | WSS: ' + wssCount;
    if (tEl) tEl.textContent = 'Total Runes: ' + totalRunes;
  }

  function toggleRuneMap(forceState) {
    const pop = document.getElementById('runePopover');
    const btn = document.getElementById('btnRuneMap');
    if (!pop || !btn) return;
    const willOpen = typeof forceState === 'boolean' ? forceState : !pop.classList.contains('open');
    if (willOpen) {
      pop.classList.add('open');
      btn.classList.add('active');
    } else {
      pop.classList.remove('open');
      btn.classList.remove('active');
    }
    try { localStorage.setItem('d2_runemap_open', willOpen ? '1' : '0'); } catch (e) {}
  }

  (function initRuneMapDrag() {
    const pop = document.getElementById('runePopover');
    const hdr = document.getElementById('runePopoverHeader');
    if (!pop || !hdr) return;

    try {
      const savedPos = JSON.parse(localStorage.getItem('d2_runemap_pos') || 'null');
      if (savedPos && typeof savedPos.left === 'number' && typeof savedPos.top === 'number') {
        pop.style.left = Math.max(8, Math.min(window.innerWidth - 120, savedPos.left)) + 'px';
        pop.style.top = Math.max(8, Math.min(window.innerHeight - 80, savedPos.top)) + 'px';
        pop.style.right = 'auto';
      }
      if (localStorage.getItem('d2_runemap_open') === '1') {
        toggleRuneMap(true);
      }
    } catch (e) {}

    let dragging = false, startX = 0, startY = 0, origLeft = 0, origTop = 0;
    hdr.addEventListener('mousedown', function(e) {
      if (e.target.tagName === 'BUTTON') return;
      dragging = true;
      const rect = pop.getBoundingClientRect();
      startX = e.clientX;
      startY = e.clientY;
      origLeft = rect.left;
      origTop = rect.top;
      pop.style.right = 'auto';
      pop.style.left = origLeft + 'px';
      pop.style.top = origTop + 'px';
      e.preventDefault();
    });
    window.addEventListener('mousemove', function(e) {
      if (!dragging) return;
      const newLeft = Math.max(0, Math.min(window.innerWidth - pop.offsetWidth, origLeft + (e.clientX - startX)));
      const newTop = Math.max(0, Math.min(window.innerHeight - pop.offsetHeight, origTop + (e.clientY - startY)));
      pop.style.left = newLeft + 'px';
      pop.style.top = newTop + 'px';
    });
    window.addEventListener('mouseup', function() {
      if (!dragging) return;
      dragging = false;
      try {
        localStorage.setItem('d2_runemap_pos', JSON.stringify({
          left: parseInt(pop.style.left, 10),
          top: parseInt(pop.style.top, 10)
        }));
      } catch (e) {}
    });
  })();

  async function api(endpoint) {
    try {
      const res = await fetch(endpoint, { method: 'POST' });
      if (!res.ok) {
        const msg = await res.text();
        console.error("API Error: " + msg);
      }
    } catch (e) {
      const box = document.getElementById('consoleBox');
      if (box) {
        box.textContent += '\n[UI Error] Could not reach local server: ' + e.message;
        box.scrollTop = box.scrollHeight;
      }
    }
    await refresh();
  }

  async function refresh() {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();

      const badge = document.getElementById('statusBadge');
      const btnStart = document.getElementById('btnStart');
      const btnStop = document.getElementById('btnStop');

      if (data.running) {
        badge.textContent = 'Running';
        badge.className = 'badge running';
        btnStart.disabled = true;
        btnStop.disabled = false;
      } else {
        badge.textContent = 'Stopped';
        badge.className = 'badge stopped';
        btnStart.disabled = false;
        btnStop.disabled = true;
      }

      const box = document.getElementById('consoleBox');
      const seq = data.log_seq !== undefined ? data.log_seq : data.logs.length;
      if (seq !== lastLogSeq) {
        box.textContent = data.logs.join('\n');
        box.scrollTop = box.scrollHeight;
        lastLogSeq = seq;
      }

      if (data.metrics) {
        const m = data.metrics;
        document.getElementById('mTotal').textContent = m.total_drops.total;
        document.getElementById('mTotalHr').textContent = m.total_drops.per_hour.toFixed(1) + ' / hr';

        document.getElementById('mRunes').textContent = m.runes_found.total;
        document.getElementById('mRunesHr').textContent = m.runes_found.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mRunesPct').textContent = m.runes_found.percentage.toFixed(9) + '%';

        document.getElementById('mHighRunes').textContent = m.high_runes.total;
        document.getElementById('mHighRunesHr').textContent = m.high_runes.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mHighRunesPct').textContent = m.high_runes.percentage.toFixed(9) + '%';

        document.getElementById('mWss').textContent = m.wss_found.total;
        document.getElementById('mWssHr').textContent = m.wss_found.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mWssPct').textContent = m.wss_found.percentage.toFixed(9) + '%';

        document.getElementById('mKeys').textContent = m.keys_found.total;
        document.getElementById('mKeysHr').textContent = m.keys_found.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mKeysPct').textContent = m.keys_found.percentage.toFixed(9) + '%';

        document.getElementById('mPuzzle').textContent = m.puzzle_found.total;
        document.getElementById('mPuzzleHr').textContent = m.puzzle_found.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mPuzzlePct').textContent = m.puzzle_found.percentage.toFixed(9) + '%';

        document.getElementById('mMirrors').textContent = m.mirrors_vials.total;
        document.getElementById('mMirrorsHr').textContent = m.mirrors_vials.per_hour.toFixed(1) + ' / hr';
        document.getElementById('mMirrorsPct').textContent = m.mirrors_vials.percentage.toFixed(9) + '%';

        renderRuneMap(m.rune_counts, m.wss_found.total);

        const locTbody = document.getElementById('locationsTable');
        if (m.top_locations && m.top_locations.length > 0) {
          locTbody.innerHTML = m.top_locations.map(l => 
            '<tr><td><strong>' + escapeHtml(l.name) + '</strong></td><td>' + l.count + '</td><td>' + l.per_hour.toFixed(1) + ' / hr</td></tr>'
          ).join('');
        } else {
          locTbody.innerHTML = '<tr><td colspan="3" style="text-align:center; color:var(--text-muted)">No drop locations recorded yet</td></tr>';
        }
      }

      const dropTbody = document.getElementById('dropsTable');
      if (data.drops && data.drops.length > 0) {
        dropTbody.innerHTML = data.drops.map(d => {
          const isRune = /\b(el|eld|tir|nef|eth|ith|tal|ral|ort|thul|amn|sol|shael|dol|hel|io|lum|ko|fal|lem|pul|um|mal|ist|gul|vex|ohm|lo|sur|ber|jah|cham|zod)\s+rune\b/i.test(d.matched_item || '');
          const itemHtml = isRune
            ? '<span class="rune-item">' + escapeHtml(d.matched_item) + '</span>'
            : '<strong>' + escapeHtml(d.matched_item) + '</strong>';
          return '<tr><td>' + escapeHtml(d.timestamp) + '</td><td>' + escapeHtml(d.location) + '</td><td>' + itemHtml + '</td><td style="color:var(--text-muted)">' + escapeHtml(d.raw_line) + '</td></tr>';
        }).join('');
      } else {
        dropTbody.innerHTML = '<tr><td colspan="4" style="text-align:center; color:var(--text-muted)">No drops recorded yet. Start your game and begin farming!</td></tr>';
      }
    } catch (e) {}
  }

  function escapeHtml(str) {
    return (str || '').replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  function setTheme(themeName) {
    document.documentElement.setAttribute('data-theme', themeName);
    try { localStorage.setItem('d2_ui_theme', themeName); } catch (e) {}
    const sel = document.getElementById('themeSelect');
    if (sel && sel.value !== themeName) sel.value = themeName;
  }

  (function initTheme() {
    let saved = 'neon';
    try { saved = localStorage.getItem('d2_ui_theme') || 'neon'; } catch (e) {}
    setTheme(saved);
  })();

  renderRuneMap({});
  setInterval(refresh, 1000);
  refresh();
</script>
</body>
</html>
`

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(getExeDir()))))

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		drops := readDropsCSV()
		metrics := calculateMetrics(drops)
		resp := map[string]interface{}{
			"running": state.running,
			"logs":    state.logs,
			"log_seq": state.logSeq,
			"drops":   drops,
			"metrics": metrics,
		}
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		if err := state.startPython(); err != nil {
			state.addLog(fmt.Sprintf("[Manager Error] %v", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		state.stopPython()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		dir := getExeDir()
		var err error
		switch runtime.GOOS {
		case "windows":
			err = exec.Command("explorer", dir).Start()
		case "darwin":
			err = exec.Command("open", dir).Start()
		default:
			err = exec.Command("xdg-open", dir).Start()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		state.stopPython()
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(250 * time.Millisecond)
			os.Exit(0)
		}()
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	fmt.Printf("D2DropManager running at %s\n", url)
	go openBrowser(url)

	_ = http.Serve(listener, mux)
}
