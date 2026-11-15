package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DropRecord represents one row from my_d2_drops.csv
type DropRecord struct {
	Timestamp string `json:"timestamp"`
	Area      string `json:"area"`
	Item      string `json:"item"`
	Raw       string `json:"raw"`
}

// MetricsSummary calculates aggregated analytics for the UI
type MetricsSummary struct {
	TotalDrops     int            `json:"total_drops"`
	RunesCount     int            `json:"runes_count"`
	HighRunesCount int            `json:"high_runes_count"`
	ShardsCount    int            `json:"shards_count"`
	PuzzleCount    int            `json:"puzzle_count"`
	KeysCount      int            `json:"keys_count"`
	OtherCount     int            `json:"other_count"`
	DropsPerDay    map[string]int `json:"drops_per_day"`
	TopLocations   []StatPair     `json:"top_locations"`
	TopItems       []StatPair     `json:"top_items"`
	Velocity       VelocityStats  `json:"velocity"`
}

type StatPair struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type VelocityStats struct {
	TotalPerHour     float64 `json:"total_per_hour"`
	RunesPerHour     float64 `json:"runes_per_hour"`
	HighRunesPerHour float64 `json:"high_runes_per_hour"`
	ShardsPerHour    float64 `json:"shards_per_hour"`
	KeysPerHour      float64 `json:"keys_per_hour"`
	ActiveHours      float64 `json:"active_hours"`
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

// getTargetDir reliably resolves to C:\Users\<username>\Desktop\tesseract or the executable folder
func getTargetDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		desktopTesseract := filepath.Join(home, "Desktop", "tesseract")
		if _, err := os.Stat(filepath.Join(desktopTesseract, "d2_screen_logger.py")); err == nil {
			return desktopTesseract
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "d2_screen_logger.py")); err == nil {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// findPythonCmd resolves the available Python executable on Windows
func findPythonCmd() string {
	for _, bin := range []string{"python", "py", "python3"} {
		if path, err := exec.LookPath(bin); err == nil {
			return path
		}
	}
	return "python"
}

func (s *AppState) startPython() error {
	s.mu.Lock()
	if s.running && s.cmd != nil && s.cmd.Process != nil {
		if err := s.cmd.Process.Signal(syscall.Signal(0)); err == nil && runtime.GOOS != "windows" {
			s.mu.Unlock()
			return fmt.Errorf("logger is already running")
		}
		if s.cmd.ProcessState != nil {
			s.running = false
			s.cmd = nil
		} else if runtime.GOOS == "windows" {
			out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", s.cmd.Process.Pid), "/NH").Output()
			if err == nil && strings.Contains(string(out), fmt.Sprintf("%d", s.cmd.Process.Pid)) {
				s.mu.Unlock()
				return fmt.Errorf("logger is already running")
			}
			s.running = false
			s.cmd = nil
		}
	}

	workDir := getTargetDir()
	scriptPath := filepath.Join(workDir, "d2_screen_logger.py")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		s.mu.Unlock()
		errMsg := fmt.Sprintf("[Manager Error] Could not find d2_screen_logger.py in %s", workDir)
		s.addLog(errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	pyBin := findPythonCmd()
	cmd := exec.Command(pyBin, "-u", scriptPath)
	cmd.Dir = workDir

	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		s.addLog(fmt.Sprintf("[Manager Error] StdoutPipe failed: %v", err))
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		s.addLog(fmt.Sprintf("[Manager Error] StderrPipe failed: %v", err))
		return err
	}

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		errMsg := fmt.Sprintf("[Manager Error] Failed to start Python (%s): %v", pyBin, err)
		s.addLog(errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	s.cmd = cmd
	s.running = true
	s.mu.Unlock()

	s.addLog(fmt.Sprintf("[Manager] Started d2_screen_logger.py using %s in %s...", filepath.Base(pyBin), workDir))

	go s.streamOutput(stdout)
	go s.streamOutput(stderr)

	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.running = false
		s.cmd = nil
		s.mu.Unlock()
		if err != nil {
			s.addLog(fmt.Sprintf("[Manager] Logger stopped (%v).", err))
		} else {
			s.addLog("[Manager] Logger stopped cleanly.")
		}
	}()

	return nil
}

func (s *AppState) streamOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.addLog(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		s.addLog(fmt.Sprintf("[Manager Warning] Output stream read error: %v", err))
	}
}

func (s *AppState) stopPython() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.cmd == nil || s.cmd.Process == nil {
		s.running = false
		s.cmd = nil
		return
	}

	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/f", "/t", "/pid", fmt.Sprintf("%d", s.cmd.Process.Pid)).Run()
	} else {
		_ = s.cmd.Process.Kill()
	}

	s.running = false
	s.cmd = nil
	s.addLog("[Manager] Logger stopped by user.")
}

func readDropsCSV() []DropRecord {
	csvPath := filepath.Join(getTargetDir(), "my_d2_drops.csv")
	file, err := os.Open(csvPath)
	if err != nil {
		return []DropRecord{}
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	rows, err := reader.ReadAll()
	if err != nil || len(rows) == 0 {
		return []DropRecord{}
	}

	startIdx := 0
	if len(rows[0]) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "timestamp") {
		startIdx = 1
	}

	var records []DropRecord
	for i := len(rows) - 1; i >= startIdx; i-- {
		row := rows[i]
		if len(row) >= 3 {
			raw := ""
			if len(row) >= 4 {
				raw = strings.Join(row[3:], ",")
			}
			records = append(records, DropRecord{
				Timestamp: strings.TrimSpace(row[0]),
				Area:      strings.TrimSpace(row[1]),
				Item:      strings.TrimSpace(row[2]),
				Raw:       raw,
			})
		}
	}
	return records
}

func calculateMetrics(drops []DropRecord) MetricsSummary {
	metrics := MetricsSummary{
		TotalDrops:   len(drops),
		DropsPerDay:  make(map[string]int),
		TopLocations: []StatPair{},
		TopItems:     []StatPair{},
	}

	locMap := make(map[string]int)
	itemMap := make(map[string]int)

	var firstTime, lastTime time.Time
	hasValidTime := false
	highRuneRegex := regexp.MustCompile(`(?i)\b(pul|um|mal|ist|gul|vex|ohm|lo|sur|ber|jah|cham|zod)\s+rune\b`)

	for _, d := range drops {
		itemLower := strings.ToLower(d.Item)
		if strings.Contains(itemLower, "rune") {
			metrics.RunesCount++
			if highRuneRegex.MatchString(d.Item) {
				metrics.HighRunesCount++
			}
		} else if strings.Contains(itemLower, "worldstone shard") {
			metrics.ShardsCount++
		} else if strings.Contains(itemLower, "puzzle") || strings.Contains(itemLower, "mirror") || strings.Contains(itemLower, "lightsong") {
			metrics.PuzzleCount++
		} else if strings.Contains(itemLower, "key of") {
			metrics.KeysCount++
		} else {
			metrics.OtherCount++
		}

		t, err := time.Parse("2006-01-02 15:04:05", d.Timestamp)
		if err == nil {
			if !hasValidTime {
				firstTime = t
				lastTime = t
				hasValidTime = true
			} else {
				if t.Before(firstTime) {
					firstTime = t
				}
				if t.After(lastTime) {
					lastTime = t
				}
			}
		}

		if len(d.Timestamp) >= 10 {
			day := d.Timestamp[:10]
			metrics.DropsPerDay[day]++
		}

		area := d.Area
		if area == "" {
			area = "Unknown Area"
		}
		locMap[area]++
		itemMap[d.Item]++
	}

	if hasValidTime && len(drops) > 0 {
		diffHours := lastTime.Sub(firstTime).Hours()
		if diffHours < 0.25 {
			diffHours = 0.25
		}
		activeHours := diffHours
		if len(drops) == 1 {
			activeHours = 1.0
		}

		metrics.Velocity = VelocityStats{
			TotalPerHour:     float64(metrics.TotalDrops) / activeHours,
			RunesPerHour:     float64(metrics.RunesCount) / activeHours,
			HighRunesPerHour: float64(metrics.HighRunesCount) / activeHours,
			ShardsPerHour:    float64(metrics.ShardsCount) / activeHours,
			KeysPerHour:      float64(metrics.KeysCount) / activeHours,
			ActiveHours:      activeHours,
		}
	}

	metrics.TopLocations = sortMapToPairs(locMap, 10)
	metrics.TopItems = sortMapToPairs(itemMap, 5)

	return metrics
}

func sortMapToPairs(m map[string]int, limit int) []StatPair {
	pairs := make([]StatPair, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, StatPair{Name: k, Count: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Count != pairs[j].Count {
			return pairs[i].Count > pairs[j].Count
		}
		return strings.ToLower(pairs[i].Name) < strings.ToLower(pairs[j].Name)
	})
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}
	return pairs
}

func openBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		drops := readDropsCSV()
		metrics := calculateMetrics(drops)
		resp := map[string]interface{}{
			"running": state.running,
			"logs":    state.logs,
			"drops":   drops,
			"metrics": metrics,
		}
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		if err := state.startPython(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		state.stopPython()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		targetDir := getTargetDir()
		_ = exec.Command("explorer", targetDir).Start()
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

	addr := "127.0.0.1:48291"
	url := "http://" + addr

	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser(url)
	}()

	fmt.Printf("D2 Drop Manager running at %s\n", url)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("Server error:", err)
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>PD2 Drop Tracker Manager &amp; Metrics</title>
<style>
  :root {
    --bg: #0e0f12;
    --card: #17191f;
    --border: #2a2d37;
    --gold: #c7a86b;
    --gold-dim: #8c7343;
    --text: #e6e6e6;
    --muted: #8b909e;
    --green: #2ea043;
    --red: #da3633;
    --orange: #e0823d;
    --purple: #a371f7;
    --blue: #58a6ff;
  }
  * { box-sizing: border-box; font-family: 'Segoe UI', system-ui, sans-serif; }
  body { background: var(--bg); color: var(--text); margin: 0; padding: 24px; }
  .container { max-width: 1300px; margin: 0 auto; }
  header {
    display: flex; justify-content: space-between; align-items: center;
    background: var(--card); border: 1px solid var(--border);
    padding: 18px 24px; border-radius: 10px; margin-bottom: 20px;
  }
  h1 { margin: 0; font-size: 20px; color: var(--gold); letter-spacing: 0.5px; }
  .subtitle { font-size: 12px; color: var(--muted); margin-top: 4px; }
  .controls { display: flex; gap: 10px; align-items: center; }
  .badge {
    padding: 6px 12px; border-radius: 20px; font-size: 12px; font-weight: 700;
    text-transform: uppercase; letter-spacing: 0.5px; margin-right: 8px;
  }
  .badge.on { background: rgba(46, 160, 67, 0.2); color: #3fb950; border: 1px solid #2ea043; }
  .badge.off { background: rgba(218, 54, 51, 0.15); color: #f85149; border: 1px solid #da3633; }
  button {
    border: none; padding: 10px 16px; border-radius: 6px; font-weight: 600;
    cursor: pointer; font-size: 13px; transition: opacity 0.15s;
  }
  button:hover { opacity: 0.88; }
  button:disabled { opacity: 0.4; cursor: not-allowed; }
  .btn-start { background: var(--green); color: #fff; }
  .btn-stop { background: var(--red); color: #fff; }
  .btn-sec { background: #262933; color: var(--text); border: 1px solid var(--border); }
  
  /* Metrics Bar */
  .metrics-grid {
    display: grid; grid-template-columns: repeat(6, 1fr); gap: 15px; margin-bottom: 20px;
  }
  @media (max-width: 1150px) { .metrics-grid { grid-template-columns: repeat(3, 1fr); } }
  @media (max-width: 650px) { .metrics-grid { grid-template-columns: repeat(2, 1fr); } }
  .metric-card {
    background: var(--card); border: 1px solid var(--border); border-radius: 8px; padding: 16px;
    display: flex; flex-direction: column;
  }
  .metric-title { font-size: 11px; font-weight: 700; color: var(--muted); text-transform: uppercase; letter-spacing: 0.5px; }
  .metric-value { font-size: 24px; font-weight: 700; color: var(--gold); margin-top: 6px; }
  .metric-sub { font-size: 11px; color: var(--muted); margin-top: 4px; }

  .grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 20px; }
  @media (max-width: 1100px) { .grid { grid-template-columns: 1fr; } }
  
  .card {
    background: var(--card); border: 1px solid var(--border);
    border-radius: 10px; padding: 18px; display: flex; flex-direction: column;
  }
  .card-header {
    display: flex; justify-content: space-between; align-items: center;
    margin-bottom: 12px; border-bottom: 1px solid var(--border); padding-bottom: 10px;
  }
  .card-title { font-size: 14px; font-weight: 700; color: var(--gold); text-transform: uppercase; }
  .console {
    background: #090a0c; border: 1px solid var(--border); border-radius: 6px;
    padding: 12px; height: 380px; overflow-y: auto; font-family: 'Consolas', monospace;
    font-size: 12px; line-height: 1.5; color: #b8c0cc; white-space: pre-wrap;
  }
  .table-wrap { height: 380px; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th {
    text-align: left; padding: 10px; background: #121419; color: var(--muted);
    position: sticky; top: 0; border-bottom: 1px solid var(--border);
  }
  td { padding: 10px; border-bottom: 1px solid #22252e; }
  .item-pill { color: var(--orange); font-weight: 700; }
  .area-tag { background: #232732; padding: 3px 8px; border-radius: 4px; font-size: 12px; color: #d0d7de; }
  
  .stat-list { display: flex; flex-direction: column; gap: 8px; height: 380px; overflow-y: auto; }
  .stat-row { display: flex; justify-content: space-between; padding: 8px 12px; background: #121419; border-radius: 6px; font-size: 13px; }
  .stat-row span:last-child { font-weight: 700; color: var(--gold); }
</style>
</head>
<body>
<div class="container">
  <header>
    <div>
      <h1>PD2 SEASON 14 DROP MANAGER &amp; METRICS</h1>
      <div class="subtitle">Real-Time OCR Screen Watcher, CSV Logger &amp; Drop Analytics</div>
    </div>
    <div class="controls">
      <span id="statusBadge" class="badge off">Stopped</span>
      <button id="btnStart" class="btn-start" onclick="api('/api/start')">Start Logger</button>
      <button id="btnStop" class="btn-stop" onclick="api('/api/stop')" disabled>Stop</button>
      <button class="btn-sec" onclick="api('/api/open-folder')">Open Folder</button>
      <button class="btn-sec" onclick="quitApp()">Exit</button>
    </div>
  </header>

  <!-- Metrics Summary Cards -->
  <div class="metrics-grid">
    <div class="metric-card">
      <span class="metric-title">Total Drops</span>
      <span id="mTotal" class="metric-value">0</span>
      <span id="vTotal" class="metric-sub">0.0 / hr</span>
    </div>
    <div class="metric-card">
      <span class="metric-title">Runes Found</span>
      <span id="mRunes" class="metric-value" style="color: var(--orange);">0</span>
      <span id="vRunes" class="metric-sub">0.0 / hr</span>
    </div>
    <div class="metric-card">
      <span class="metric-title">Mid-High Runes (Pul+)</span>
      <span id="mHighRunes" class="metric-value" style="color: #ff7b72;">0</span>
      <span id="vHighRunes" class="metric-sub">0.0 / hr</span>
    </div>
    <div class="metric-card">
      <span class="metric-title">Worldstone Shards</span>
      <span id="mShards" class="metric-value" style="color: var(--purple);">0</span>
      <span id="vShards" class="metric-sub">0.0 / hr</span>
    </div>
    <div class="metric-card">
      <span class="metric-title">Keys Found</span>
      <span id="mKeys" class="metric-value" style="color: var(--gold);">0</span>
      <span id="vKeys" class="metric-sub">0.0 / hr</span>
    </div>
    <div class="metric-card">
      <span class="metric-title">Puzzle Boxes / Mirrors</span>
      <span id="mPuzzles" class="metric-value" style="color: var(--blue);">0</span>
    </div>
  </div>

  <div class="grid">
    <!-- Live Console -->
    <div class="card">
      <div class="card-header">
        <span class="card-title">Live Scanner Feed</span>
      </div>
      <div id="consoleBox" class="console">Connecting...</div>
    </div>

    <!-- Analytics Breakdown -->
    <div class="card">
      <div class="card-header">
        <span class="card-title">Top Drop Locations</span>
      </div>
      <div id="topLocationsBox" class="stat-list">
        <div style="color:var(--muted); text-align:center; padding:20px;">No location data yet.</div>
      </div>
    </div>

    <!-- Logged Drops Table -->
    <div class="card">
      <div class="card-header">
        <span class="card-title">Drop History</span>
      </div>
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Item</th>
              <th>Area / Map</th>
            </tr>
          </thead>
          <tbody id="dropsBody">
            <tr><td colspan="3" style="color:var(--muted); text-align:center;">No drops logged yet.</td></tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</div>

<script>
  let lastLogCount = 0;

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
        box.textContent += '\n[UI Error] Could not reach local server (try refreshing the page): ' + e.message;
        box.scrollTop = box.scrollHeight;
      }
    }
    refresh();
  }

  async function quitApp() {
    await fetch('/api/quit', { method: 'POST' });
    document.body.innerHTML = "<div style='color:#c7a86b;text-align:center;margin-top:20%;font-family:sans-serif;'><h2>Manager Closed. You can close this browser tab.</h2></div>";
  }

  async function refresh() {
    try {
      const res = await fetch('/api/status');
      const data = await res.json();

      const badge = document.getElementById('statusBadge');
      const btnStart = document.getElementById('btnStart');
      const btnStop = document.getElementById('btnStop');
      if (data.running) {
        badge.className = 'badge on';
        badge.textContent = 'Watching Screen';
        btnStart.disabled = true;
        btnStop.disabled = false;
      } else {
        badge.className = 'badge off';
        badge.textContent = 'Stopped';
        btnStart.disabled = false;
        btnStop.disabled = true;
      }

      const box = document.getElementById('consoleBox');
      if (data.logs.length !== lastLogCount) {
        box.textContent = data.logs.join('\n');
        box.scrollTop = box.scrollHeight;
        lastLogCount = data.logs.length;
      }

      const m = data.metrics || {};
      const v = m.velocity || {};

      document.getElementById('mTotal').textContent = m.total_drops || 0;
      document.getElementById('vTotal').textContent = (v.total_per_hour || 0).toFixed(1) + ' / hr';

      document.getElementById('mRunes').textContent = m.runes_count || 0;
      document.getElementById('vRunes').textContent = (v.runes_per_hour || 0).toFixed(1) + ' / hr';

      document.getElementById('mHighRunes').textContent = m.high_runes_count || 0;
      document.getElementById('vHighRunes').textContent = (v.high_runes_per_hour || 0).toFixed(1) + ' / hr';

      document.getElementById('mShards').textContent = m.shards_count || 0;
      document.getElementById('vShards').textContent = (v.shards_per_hour || 0).toFixed(1) + ' / hr';

      document.getElementById('mKeys').textContent = m.keys_count || 0;
      document.getElementById('vKeys').textContent = (v.keys_per_hour || 0).toFixed(1) + ' / hr';

      document.getElementById('mPuzzles').textContent = m.puzzle_count || 0;

      const locBox = document.getElementById('topLocationsBox');
      const locs = m.top_locations || [];
      if (locs.length === 0) {
        locBox.innerHTML = '<div style="color:var(--muted); text-align:center; padding:20px;">No location data yet.</div>';
      } else {
        locBox.innerHTML = locs.map(l => 
          '<div class="stat-row"><span>' + escapeHtml(l.name) + '</span><span>' + l.count + ' drops</span></div>'
        ).join('');
      }

      const drops = data.drops || [];
      const tbody = document.getElementById('dropsBody');
      if (drops.length === 0) {
        tbody.innerHTML = '<tr><td colspan="3" style="color:var(--muted); text-align:center;">No drops logged yet.</td></tr>';
      } else {
        tbody.innerHTML = drops.map(d => 
          '<tr>' +
            '<td style="color:var(--muted); font-size:12px;">' + escapeHtml(d.timestamp) + '</td>' +
            '<td class="item-pill">' + escapeHtml(d.item.toUpperCase()) + '</td>' +
            '<td><span class="area-tag">' + escapeHtml(d.area) + '</span></td>' +
          '</tr>'
        ).join('');
      }
    } catch (e) {
      console.error(e);
    }
  }

  function escapeHtml(str) {
    return str.replace(/[&<>"']/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));
  }

  setInterval(refresh, 1000);
  refresh();
</script>
</body>
</html>`
