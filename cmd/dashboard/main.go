// dashboard: lightweight web UI for the arb bot.
// Serves JSON APIs over the SQLite database (including competitor_arbs)
// and streams logs via SSE.
//
// Config via env vars:
//   ARB_DB_PATH     (default: /home/arbitrator/go/arb-bot/arb.db)
//   ARB_LOG_FILE    (default: /tmp/arb-bot.log)
//   DASHBOARD_PORT  (default: 8080)
package main

import (
	"bufio"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"arb-bot/internal"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

//go:embed static
var staticFiles embed.FS

var (
	dbPath       = getenv("ARB_DB_PATH", "/home/arbitrator/go/arb-bot/arb.db")
	logFile      = getenv("ARB_LOG_FILE", "/home/arbitrator/go/arb-bot/bot.log")
	configPath   = getenv("ARB_CONFIG_PATH", "/home/arbitrator/go/arb-bot/config.yaml")
	port         = getenv("DASHBOARD_PORT", "8080")
	botDebugURL  = getenv("BOT_DEBUG_URL", "http://127.0.0.1:6060")
)

// proxyToBotDebug returns a handler that forwards the request to the bot's
// read-only debug HTTP server (bound to loopback only — the bot and dashboard
// must live on the same host). The URL query string is passed through so
// upstream filters like ?limit=&token=&pool= still work.
func proxyToBotDebug(path string) http.HandlerFunc {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		url := botDebugURL + path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}
		resp, err := client.Get(url)
		if err != nil {
			jsonErr(w, "bot debug server unreachable ("+botDebugURL+"): "+err.Error(), 503)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openDB() (*sql.DB, error) {
	dsn := dbPath + "?_pragma=journal_mode=wal&_pragma=synchronous=NORMAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	// Ensure any tables added after the initial schema exist.
	db.Exec(`CREATE TABLE IF NOT EXISTS competitor_arbs (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		seen_at          INTEGER NOT NULL,
		block_number     INTEGER NOT NULL,
		tx_hash          TEXT UNIQUE NOT NULL,
		sender           TEXT NOT NULL,
		bot_contract     TEXT NOT NULL,
		flash_loan_src   TEXT NOT NULL,
		borrow_token     TEXT NOT NULL DEFAULT '',
		borrow_symbol    TEXT NOT NULL DEFAULT '',
		borrow_amount    TEXT NOT NULL DEFAULT '',
		path_str         TEXT NOT NULL,
		hop_count        INTEGER NOT NULL,
		dexes            TEXT NOT NULL,
		profit_usd       REAL NOT NULL DEFAULT 0,
		net_usd          REAL NOT NULL DEFAULT 0,
		gas_used         INTEGER NOT NULL DEFAULT 0,
		hops_json        TEXT NOT NULL DEFAULT '[]'
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_comp_seen_at ON competitor_arbs(seen_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_comp_sender  ON competitor_arbs(sender)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_comp_block   ON competitor_arbs(block_number)`)
	// Defensive migrations — the bot also runs these in OpenDB, but we add
	// them here so the dashboard can start and query the columns even if the
	// bot hasn't run its migration yet (e.g. new deploy, bot still stopped).
	db.Exec(`ALTER TABLE competitor_arbs ADD COLUMN cycle_in_memory INTEGER NOT NULL DEFAULT -1`)
	db.Exec(`ALTER TABLE competitor_arbs ADD COLUMN cycle_in_memory_at INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN volume_24h_usd REAL NOT NULL DEFAULT 0`)
	db.Exec(`CREATE TABLE IF NOT EXISTS bot_health (
		id              INTEGER PRIMARY KEY DEFAULT 1 CHECK(id=1),
		updated_at      INTEGER NOT NULL,
		tick_data_at    INTEGER NOT NULL DEFAULT 0,
		multicall_at    INTEGER NOT NULL DEFAULT 0,
		swap_event_at   INTEGER NOT NULL DEFAULT 0,
		cycle_rebuild_at INTEGER NOT NULL DEFAULT 0
	)`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN ticked_pools_have  INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN ticked_pools_total INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN tracked_pool_addrs TEXT NOT NULL DEFAULT '[]'`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN rpc_state TEXT NOT NULL DEFAULT '[]'`)
	return db, nil
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func intParam(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	db, err := openDB()
	if err != nil {
		log.Fatalf("open db %s: %v", dbPath, err)
	}
	defer db.Close()

	// Pre-load address→symbol map once; refresh every 5 min in background.
	// RWMutex protects against data race between the refresh goroutine and HTTP handlers.
	var (
		addrToSymMu sync.RWMutex
		addrToSym   = loadAddrToSymbol(db)
	)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		for range t.C {
			m := loadAddrToSymbol(db)
			addrToSymMu.Lock()
			addrToSym = m
			addrToSymMu.Unlock()
		}
	}()

	staticFS, _ := fs.Sub(staticFiles, "static")
	http.Handle("/", http.FileServer(http.FS(staticFS)))

	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) { apiStats(db, w, r) })
	http.HandleFunc("/api/trades", func(w http.ResponseWriter, r *http.Request) {
		addrToSymMu.RLock()
		m := addrToSym
		addrToSymMu.RUnlock()
		apiTrades(db, w, r, m)
	})
	http.HandleFunc("/api/observations", func(w http.ResponseWriter, r *http.Request) {
		addrToSymMu.RLock()
		m := addrToSym
		addrToSymMu.RUnlock()
		apiObservations(db, w, r, m)
	})
	http.HandleFunc("/api/competitors", apiCompetitors(db))
	http.HandleFunc("/api/competitor-comparison", apiCompetitorComparison(dbPath))
	// Proxy endpoints to the bot's loopback-only debug HTTP server (127.0.0.1:6060).
	// The cycle cache lives in the bot process memory, so the dashboard has to
	// ask the bot for it. We expose /api/cycles and /api/cycle-stats which
	// forward to /debug/cycles and /debug/cycle-cache/stats respectively.
	http.HandleFunc("/api/cycles", proxyToBotDebug("/debug/cycles"))
	http.HandleFunc("/api/cycle-stats", proxyToBotDebug("/debug/cycle-cache/stats"))
	http.HandleFunc("/api/contracts", func(w http.ResponseWriter, r *http.Request) { apiContracts(db, w, r) })
	http.HandleFunc("/api/logs", apiLogs)
	http.HandleFunc("/api/logs/stream", apiLogsStream)
	http.HandleFunc("/api/config", apiConfig)
	http.HandleFunc("/api/processes", apiProcesses)
	http.HandleFunc("/api/query", func(w http.ResponseWriter, r *http.Request) { apiQuery(db, w, r) })
	http.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { apiHealth(db, w) })
	http.HandleFunc("/api/pools", func(w http.ResponseWriter, r *http.Request) { apiPools(db, w, r) })
	http.HandleFunc("/api/trade", func(w http.ResponseWriter, r *http.Request) { apiTradeDetail(db, w, r, addrToSym) })

	log.Printf("Dashboard on :%s  db=%s  log=%s", port, dbPath, logFile)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// ─── /api/stats ──────────────────────────────────────────────────────────────

func apiStats(db *sql.DB, w http.ResponseWriter, _ *http.Request) {
	type Stats struct {
		TotalTrades         int     `json:"total_trades"`
		SuccessTrades       int     `json:"success_trades"`
		RevertedTrades      int     `json:"reverted_trades"`
		PendingTrades       int     `json:"pending_trades"`
		TotalProfitUSD      float64 `json:"total_profit_usd"`
		TotalObservations   int     `json:"total_observations"`
		ExecutedObs         int     `json:"executed_observations"`
		Last24hObservations int     `json:"last_24h_observations"`
		Last24hTrades       int     `json:"last_24h_trades"`
	}
	var s Stats
	// Use strftime to get unix timestamp for 24h ago directly in SQLite
	db.QueryRow(`SELECT COUNT(*) FROM our_trades`).Scan(&s.TotalTrades)
	db.QueryRow(`SELECT COUNT(*) FROM our_trades WHERE status='success'`).Scan(&s.SuccessTrades)
	db.QueryRow(`SELECT COUNT(*) FROM our_trades WHERE status='reverted'`).Scan(&s.RevertedTrades)
	db.QueryRow(`SELECT COUNT(*) FROM our_trades WHERE status='pending'`).Scan(&s.PendingTrades)
	db.QueryRow(`SELECT COALESCE(SUM(profit_usd_est),0) FROM our_trades WHERE status='success'`).Scan(&s.TotalProfitUSD)
	db.QueryRow(`SELECT COUNT(*) FROM arb_observations`).Scan(&s.TotalObservations)
	db.QueryRow(`SELECT COUNT(*) FROM arb_observations WHERE executed=1`).Scan(&s.ExecutedObs)
	db.QueryRow(`SELECT COUNT(*) FROM arb_observations WHERE observed_at > strftime('%s','now','-24 hours')`).Scan(&s.Last24hObservations)
	db.QueryRow(`SELECT COUNT(*) FROM our_trades WHERE submitted_at > strftime('%s','now','-24 hours')`).Scan(&s.Last24hTrades)
	jsonOK(w, s)
}

// ─── /api/trades ─────────────────────────────────────────────────────────────

// loadAddrToSymbol loads the full address→symbol map from the tokens table.
func loadAddrToSymbol(db *sql.DB) map[string]string {
	m := make(map[string]string)
	rows, err := db.Query(`SELECT address, symbol FROM tokens`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var addr, sym string
		if rows.Scan(&addr, &sym) == nil {
			m[strings.ToLower(addr)] = sym
		}
	}
	return m
}

// resolveHopPrices converts an address-keyed hop_prices_json string to a
// symbol-keyed JSON string using the provided address→symbol map.
func resolveHopPrices(hopPricesJSON string, addrToSym map[string]string) string {
	if hopPricesJSON == "" || hopPricesJSON == "{}" {
		return "{}"
	}
	var addrPrices map[string]float64
	if err := json.Unmarshal([]byte(hopPricesJSON), &addrPrices); err != nil {
		return "{}"
	}
	symPrices := make(map[string]float64)
	for addr, price := range addrPrices {
		if sym, ok := addrToSym[strings.ToLower(addr)]; ok {
			symPrices[strings.ToLower(sym)] = price
		}
	}
	b, _ := json.Marshal(symPrices)
	return string(b)
}

func apiTrades(db *sql.DB, w http.ResponseWriter, r *http.Request, addrToSym map[string]string) {
	limit := intParam(r, "limit", 50)
	offset := intParam(r, "offset", 0)
	status := r.URL.Query().Get("status")

	type Trade struct {
		ID                int64    `json:"id"`
		SubmittedAt       int64    `json:"submitted_at"`
		TxHash            string   `json:"tx_hash"`
		Status            string   `json:"status"`
		BlockNumber       *int64   `json:"block_number"`
		BlockPosition     *int64   `json:"block_position"`
		Hops              int      `json:"hops"`
		ProfitUSDEst      float64  `json:"profit_usd_est"`
		Dexes             string   `json:"dexes"`
		Tokens            string   `json:"tokens"`
		GasUsed           *int64   `json:"gas_used"`
		CompetitorsBefore *int     `json:"competitors_before"`
		DetectRPC         string   `json:"detect_rpc"`
		HopPricesJSON     string   `json:"hop_prices_json"`
	}

	query := `SELECT id, submitted_at, tx_hash, status, block_number, block_position,
	          hops, profit_usd_est, dexes, tokens, gas_used, competitors_before, detect_rpc, hop_prices_json
	          FROM our_trades`
	args := []interface{}{}
	if status != "" {
		query += " WHERE status=?"
		args = append(args, status)
	}
	query += " ORDER BY submitted_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	trades := []Trade{}
	for rows.Next() {
		var t Trade
		var rawPrices string
		if err := rows.Scan(&t.ID, &t.SubmittedAt, &t.TxHash, &t.Status,
			&t.BlockNumber, &t.BlockPosition, &t.Hops, &t.ProfitUSDEst,
			&t.Dexes, &t.Tokens, &t.GasUsed, &t.CompetitorsBefore, &t.DetectRPC, &rawPrices); err != nil {
			continue
		}
		t.HopPricesJSON = resolveHopPrices(rawPrices, addrToSym)
		trades = append(trades, t)
	}
	jsonOK(w, trades)
}

// ─── /api/observations ───────────────────────────────────────────────────────

func apiObservations(db *sql.DB, w http.ResponseWriter, r *http.Request, addrToSym map[string]string) {
	limit := intParam(r, "limit", 100)
	offset := intParam(r, "offset", 0)
	executed := r.URL.Query().Get("executed")

	type Obs struct {
		ID             int64   `json:"id"`
		ObservedAt     int64   `json:"observed_at"`
		Hops           int     `json:"hops"`
		ProfitPct      float64 `json:"profit_pct"`
		ProfitUSD      float64 `json:"profit_usd"`
		GasCostUSD     float64 `json:"gas_cost_usd"`
		NetProfitUSD   float64 `json:"net_profit_usd"`
		Dexes          string  `json:"dexes"`
		Tokens         string  `json:"tokens"`
		Executed       int     `json:"executed"`
		TxHash         *string `json:"tx_hash"`
		RejectReason   string  `json:"reject_reason"`
		ArbType        string  `json:"arb_type"`
		HopPricesJSON  string  `json:"hop_prices_json"`
	}

	rejected := r.URL.Query().Get("rejected")
	// verdict filters the observation list by eth_call outcome:
	//   ok         → only rows where reject_reason contains "eth_call OK"
	//                (cycles that would have SUBMITTED + SUCCEEDED if live)
	//   reverted   → only "REVERTED even at 99%slippage" — the phantom class
	//   slip_over  → "REVERTED at Xbps but OK at 99%%" — slippage overestimate
	//   phantom    → classifier tag [SIM_PHANTOM: ...]
	//   latency    → classifier tag [LATENCY_DRIFT: ...]
	// Unspecified/empty → no filter.
	verdict := r.URL.Query().Get("verdict")
	query := `SELECT id, observed_at, hops, profit_pct, profit_usd, net_profit_usd, gas_cost_usd, dexes, tokens, executed, tx_hash, reject_reason, arb_type, hop_prices_json
	          FROM arb_observations`
	args := []interface{}{}
	wheres := []string{}
	if executed == "0" || executed == "1" {
		wheres = append(wheres, "executed=?")
		args = append(args, executed)
	} else if rejected == "1" {
		wheres = append(wheres, "reject_reason != ''")
	}
	switch verdict {
	case "ok":
		wheres = append(wheres, "reject_reason LIKE '%eth_call OK%'")
	case "reverted":
		wheres = append(wheres, "reject_reason LIKE '%REVERTED even%'")
	case "slip_over":
		wheres = append(wheres, "reject_reason LIKE '%OK at 99%%'")
	case "phantom":
		wheres = append(wheres, "reject_reason LIKE '%[SIM_PHANTOM:%'")
	case "latency":
		wheres = append(wheres, "reject_reason LIKE '%[LATENCY_DRIFT:%'")
	case "v4_handler":
		wheres = append(wheres, "reject_reason LIKE '%[V4_HANDLER:%'")
	case "predryrun":
		wheres = append(wheres, "reject_reason LIKE '%pre-dryrun%'")
	case "health":
		wheres = append(wheres, "reject_reason LIKE '%health%'")
	case "no_executor":
		wheres = append(wheres, "reject_reason LIKE '%no-executor%'")
	}
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	query += " ORDER BY observed_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	obs := []Obs{}
	for rows.Next() {
		var o Obs
		var rawPrices string
		if err := rows.Scan(&o.ID, &o.ObservedAt, &o.Hops, &o.ProfitPct, &o.ProfitUSD, &o.NetProfitUSD, &o.GasCostUSD,
			&o.Dexes, &o.Tokens, &o.Executed, &o.TxHash, &o.RejectReason, &o.ArbType, &rawPrices); err != nil {
			continue
		}
		o.HopPricesJSON = resolveHopPrices(rawPrices, addrToSym)
		obs = append(obs, o)
	}
	jsonOK(w, obs)
}

// ─── /api/competitors ────────────────────────────────────────────────────────

func apiCompetitors(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 100)
		offset := intParam(r, "offset", 0)
		sender := r.URL.Query().Get("sender")
		arbType := r.URL.Query().Get("arb_type")
		profitable := r.URL.Query().Get("profitable")
		flashSrc := r.URL.Query().Get("flash_loan_src")

		type Competitor struct {
			ID                int64   `json:"id"`
			SeenAt            int64   `json:"seen_at"`
			BlockNumber       int64   `json:"block_number"`
			TxHash            string  `json:"tx_hash"`
			Sender            string  `json:"sender"`
			BotContract       string  `json:"bot_contract"`
			FlashLoanSrc      string  `json:"flash_loan_source"`
			BorrowSymbol      string  `json:"borrow_symbol"`
			BorrowAmount      string  `json:"borrow_amount_human"`
			PathStr           string  `json:"path_str"`
			HopCount          int     `json:"hop_count"`
			Dexes             string  `json:"dexes"`
			ProfitUSD         float64 `json:"profit_usd"`
			NetUSD            float64 `json:"net_profit_usd"`
			GasUsed           int64   `json:"gas_used"`
			ArbType           string  `json:"arb_type"`
			HopsJSON          string  `json:"hops_json"`
			HopPricesJSON     string  `json:"hop_prices_json"`
			ComparisonResult  string  `json:"comparison_result"`
			CycleInMemory     int     `json:"cycle_in_memory"`    // -1 unknown, 0 no, 1 yes
			CycleInMemoryAt   int64   `json:"cycle_in_memory_at"` // unix ts of determination
			// Phase 1 comparison fields (arbscan populates these at insert)
			NotionalUSD            float64 `json:"notional_usd"`
			ProfitBps              float64 `json:"profit_bps"`
			GasPriceGwei           float64 `json:"gas_price_gwei"`
			GasCostUSD             float64 `json:"gas_cost_usd"`
			BlockPosition          int     `json:"block_position"`
			BlockTotalTxs          int     `json:"block_total_txs"`
			OurMinProfitUSDAtBlock float64 `json:"our_min_profit_usd_at_block"`
			OurLPFloorBps          float64 `json:"our_lpfloor_bps"`
		}

		query := `SELECT id, seen_at, block_number, tx_hash, sender, bot_contract,
		          flash_loan_src, borrow_symbol, borrow_amount, path_str, hop_count,
		          dexes, profit_usd, net_usd, gas_used, arb_type, hops_json, hop_prices_json,
		          COALESCE(comparison_result,''), COALESCE(cycle_in_memory,-1), COALESCE(cycle_in_memory_at,0),
		          COALESCE(notional_usd,0), COALESCE(profit_bps,0), COALESCE(gas_price_gwei,0),
		          COALESCE(gas_cost_usd,0), COALESCE(block_position,0), COALESCE(block_total_txs,0),
		          COALESCE(our_min_profit_usd_at_block,0), COALESCE(our_lpfloor_bps,0)
		          FROM competitor_arbs`
		args := []interface{}{}
		wheres := []string{}
		if sender != "" {
			wheres = append(wheres, "sender=?")
			args = append(args, sender)
		}
		if arbType != "" {
			wheres = append(wheres, "arb_type=?")
			args = append(args, arbType)
		}
		if flashSrc != "" {
			wheres = append(wheres, "flash_loan_src=?")
			args = append(args, flashSrc)
		}
		if profitable == "1" {
			wheres = append(wheres, "net_usd > 0")
		} else if profitable == "0" {
			wheres = append(wheres, "net_usd <= 0")
		}
		if len(wheres) > 0 {
			query += " WHERE " + strings.Join(wheres, " AND ")
		}
		query += " ORDER BY seen_at DESC LIMIT ? OFFSET ?"
		args = append(args, limit, offset)

		rows, err := db.Query(query, args...)
		if err != nil {
			jsonErr(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		comps := []Competitor{}
		for rows.Next() {
			var c Competitor
			if err := rows.Scan(&c.ID, &c.SeenAt, &c.BlockNumber, &c.TxHash, &c.Sender,
				&c.BotContract, &c.FlashLoanSrc, &c.BorrowSymbol, &c.BorrowAmount,
				&c.PathStr, &c.HopCount, &c.Dexes, &c.ProfitUSD, &c.NetUSD, &c.GasUsed, &c.ArbType,
				&c.HopsJSON, &c.HopPricesJSON, &c.ComparisonResult,
				&c.CycleInMemory, &c.CycleInMemoryAt,
				&c.NotionalUSD, &c.ProfitBps, &c.GasPriceGwei, &c.GasCostUSD,
				&c.BlockPosition, &c.BlockTotalTxs,
				&c.OurMinProfitUSDAtBlock, &c.OurLPFloorBps); err != nil {
				continue
			}
			comps = append(comps, c)
		}
		jsonOK(w, comps)
	}
}

// ─── /api/competitor-comparison ──────────────────────────────────────────────
//
// Returns the full classification detail for one competitor_arbs row, used by
// the dashboard popup. The detail JSON includes (when applicable) the matched
// observation/trade ID, pool overlap percentage, reject reason, missing pool
// addresses, etc. — same data the comparison loop wrote to comparison_detail.

func apiCompetitorComparison(dbPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonErr(w, "missing or invalid id", 400)
			return
		}
		// Open via internal.DB to reuse the LoadCompetitorComparison helper.
		idb, err := internal.OpenDB(dbPath)
		if err != nil {
			jsonErr(w, "db open: "+err.Error(), 500)
			return
		}
		defer idb.Close()
		c, err := idb.LoadCompetitorComparison(id)
		if err != nil {
			jsonErr(w, "load: "+err.Error(), 500)
			return
		}
		if c == nil {
			jsonErr(w, "not found", 404)
			return
		}
		jsonOK(w, c)
	}
}

// ─── /api/logs ───────────────────────────────────────────────────────────────

func apiLogs(w http.ResponseWriter, r *http.Request) {
	n := intParam(r, "lines", 400)
	data, err := readLastLines(logFile, n)
	if err != nil {
		cmd := exec.Command("journalctl", "-u", "arb-bot", "-n", strconv.Itoa(n), "--no-pager")
		data, err = cmd.Output()
		if err != nil {
			jsonErr(w, "no log source available", 404)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func readLastLines(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, _ := f.Stat()
	size := stat.Size()
	if size == 0 {
		return nil, nil
	}

	const chunk = 64 * 1024
	var buf []byte
	count := 0
	pos := size

	for pos > 0 && count <= n+1 {
		read := int64(chunk)
		if pos < read {
			read = pos
		}
		pos -= read
		block := make([]byte, read)
		f.ReadAt(block, pos)
		for _, b := range block {
			if b == '\n' {
				count++
			}
		}
		buf = append(block, buf...)
	}

	lines := strings.Split(string(buf), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// ─── /api/logs/stream (SSE) ──────────────────────────────────────────────────

func apiLogsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	var cmd *exec.Cmd
	if _, err := os.Stat(logFile); err == nil {
		cmd = exec.CommandContext(r.Context(), "tail", "-f", "-n", "0", logFile)
	} else {
		cmd = exec.CommandContext(r.Context(), "journalctl", "-u", "arb-bot", "-f", "-n", "0", "--no-pager")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "data: [error] %v\n\n", err)
		flusher.Flush()
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "data: [error] %v\n\n", err)
		flusher.Flush()
		return
	}

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		select {
		case <-r.Context().Done():
			cmd.Process.Kill()
			return
		default:
		}
		line := strings.ReplaceAll(sc.Text(), "\n", " ")
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}
	cmd.Process.Kill()
}

// ─── /api/config ─────────────────────────────────────────────────────────────

func apiConfig(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		jsonErr(w, "cannot read config: "+err.Error(), 500)
		return
	}
	var parsed interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		jsonErr(w, "cannot parse config: "+err.Error(), 500)
		return
	}
	jsonOK(w, parsed)
}

// ─── /api/processes ──────────────────────────────────────────────────────────

type ProcessInfo struct {
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Running bool   `json:"running"`
	PID     string `json:"pid"`
	CPU     string `json:"cpu"`
	Mem     string `json:"mem"`
	Uptime  string `json:"uptime"`
	Args    string `json:"args"`
}

// knownProcesses defines what to look for.
// comm is matched exactly against the ps comm field (executable basename).
// pattern is matched against the full args string as a fallback/disambiguator.
var knownProcesses = []struct {
	name    string
	desc    string
	comm    string // exact match on ps comm column
	pattern string // substring match on args (used when comm alone is ambiguous)
}{
	{"arb-bot", "Arbitrage bot (main)", "bot", ""},
	{"arb-dashboard", "Web dashboard", "arb-dashboard", "arb-dashboard"},
	{"arb-scan", "Competitor scanner (arbscan)", "arbscan", ""},
	{"nitro", "Arbitrum Nitro node", "nitro", ""},
}

func apiProcesses(w http.ResponseWriter, _ *http.Request) {
	out, err := exec.Command("ps", "ax", "-o", "pid,pcpu,pmem,etimes,comm,args").Output()
	if err != nil {
		jsonErr(w, "ps failed: "+err.Error(), 500)
		return
	}

	lines := strings.Split(string(out), "\n")

	result := make([]ProcessInfo, len(knownProcesses))
	for i, kp := range knownProcesses {
		info := ProcessInfo{Name: kp.name, Desc: kp.desc}
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			// fields[4] is comm (executable basename)
			if fields[4] != kp.comm {
				continue
			}
			// if a pattern is set, also require it in the args
			args := strings.Join(fields[5:], " ")
			if kp.pattern != "" && !strings.Contains(args, kp.pattern) {
				continue
			}
			info.Running = true
			info.PID = fields[0]
			// ps -o pcpu reports per-core utilization (100% = one full core),
			// not system-wide. Normalize to system-wide so the dashboard reads
			// intuitively, but keep the raw "cores busy" value visible because
			// "1.32 / 16 cores" is more informative than "8.3%" for spotting
			// when a hot goroutine is pinning a single core.
			if rawPcpu, perr := strconv.ParseFloat(fields[1], 64); perr == nil {
				numCPU := runtime.NumCPU()
				cores := rawPcpu / 100.0
				sysPct := rawPcpu / float64(numCPU)
				info.CPU = fmt.Sprintf("%.1f%% (%.2f/%d cores)", sysPct, cores, numCPU)
			} else {
				info.CPU = fields[1] + "%"
			}
			info.Mem = fields[2] + "%"
			if secs, err := strconv.Atoi(fields[3]); err == nil {
				info.Uptime = formatUptime(secs)
			}
			// Resolve the absolute binary path from /proc/<pid>/exe (symlink to
			// the on-disk file). This shows the real path regardless of how the
			// process was launched (./bot vs /home/.../bot vs symlinked).
			// Falls back to the raw args from `ps` if the symlink can't be read.
			displayArgs := args
			if exe, err := os.Readlink("/proc/" + fields[0] + "/exe"); err == nil && exe != "" {
				// Append any extra args after the binary path so we still see flags.
				if len(fields) > 5 {
					if rest := strings.Join(fields[6:], " "); rest != "" {
						displayArgs = exe + " " + rest
					} else {
						displayArgs = exe
					}
				} else {
					displayArgs = exe
				}
			}
			if len(displayArgs) > 120 {
				displayArgs = displayArgs[:120] + "…"
			}
			info.Args = displayArgs
			break
		}
		result[i] = info
	}
	jsonOK(w, result)
}

func formatUptime(secs int) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	} else if secs < 3600 {
		return fmt.Sprintf("%dm %ds", secs/60, secs%60)
	} else if secs < 86400 {
		return fmt.Sprintf("%dh %dm", secs/3600, (secs%3600)/60)
	}
	return fmt.Sprintf("%dd %dh", secs/86400, (secs%86400)/3600)
}

// ─── /api/pools ──────────────────────────────────────────────────────────────

func apiPools(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	offset := intParam(r, "offset", 0)
	dex := r.URL.Query().Get("dex")
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "tvl_usd"
	}
	// Whitelist sort columns to prevent SQL injection
	allowed := map[string]bool{"tvl_usd": true, "volume_24h_usd": true, "first_seen": true, "last_updated": true, "fee_bps": true}
	if !allowed[sortBy] {
		sortBy = "tvl_usd"
	}

	type Pool struct {
		Address       string  `json:"address"`
		DEX           string  `json:"dex"`
		Token0        string  `json:"token0"`
		Token1        string  `json:"token1"`
		Symbol0       string  `json:"symbol0"`
		Symbol1       string  `json:"symbol1"`
		FeeBps        int     `json:"fee_bps"`
		FeePPM        int     `json:"fee_ppm"`
		TVLUSD        float64 `json:"tvl_usd"`
		Volume24hUSD  float64 `json:"volume_24h_usd"`
		SpotPrice     float64 `json:"spot_price"`
		IsStable      int     `json:"is_stable"`
		Disabled      int     `json:"disabled"`
		IsDead        int     `json:"is_dead"`
		FirstSeen     int64   `json:"first_seen"`
		LastUpdated   int64   `json:"last_updated"`
	}

	// UNION pools table (V2/V3) with v4_pools (UniV4 hooks-based pools).
	// V4 pools are keyed by pool_id (bytes32 hex) instead of an address.
	query := `SELECT * FROM (
		SELECT p.address AS address, p.dex AS dex, p.token0 AS token0, p.token1 AS token1,
			COALESCE(t0.symbol,'') AS symbol0, COALESCE(t1.symbol,'') AS symbol1,
			p.fee_bps AS fee_bps, p.fee_ppm AS fee_ppm, p.tvl_usd AS tvl_usd, p.volume_24h_usd AS volume_24h_usd, p.spot_price AS spot_price,
			p.is_stable AS is_stable, p.disabled AS disabled, p.is_dead AS is_dead, p.first_seen AS first_seen, p.last_updated AS last_updated
		FROM pools p
		LEFT JOIN tokens t0 ON t0.address = p.token0
		LEFT JOIN tokens t1 ON t1.address = p.token1
		WHERE p.dex != 'UniV4'
		UNION ALL
		SELECT v.pool_id AS address, 'UniV4' AS dex, v.token0 AS token0, v.token1 AS token1,
			COALESCE(t0.symbol,'') AS symbol0, COALESCE(t1.symbol,'') AS symbol1,
			(v.fee_ppm / 100) AS fee_bps, v.fee_ppm AS fee_ppm, v.tvl_usd AS tvl_usd, v.volume_24h_usd AS volume_24h_usd, v.spot_price AS spot_price,
			0 AS is_stable, v.disabled AS disabled, 0 AS is_dead, v.last_updated AS first_seen, v.last_updated AS last_updated
		FROM v4_pools v
		LEFT JOIN tokens t0 ON t0.address = v.token0
		LEFT JOIN tokens t1 ON t1.address = v.token1
	) p`

	wheres := []string{}
	args := []interface{}{}
	if dex != "" {
		wheres = append(wheres, "p.dex = ?")
		args = append(args, dex)
	}
	// Optional filter: comma-separated list of pool addresses (lowercased server-side).
	// Used by the "click Ticks → show tracked pools" navigation in the dashboard.
	if addrs := r.URL.Query().Get("addresses"); addrs != "" {
		raw := strings.Split(addrs, ",")
		clean := make([]string, 0, len(raw))
		for _, a := range raw {
			a = strings.ToLower(strings.TrimSpace(a))
			if a != "" {
				clean = append(clean, a)
			}
		}
		if len(clean) > 0 {
			placeholders := strings.Repeat("?,", len(clean))
			placeholders = placeholders[:len(placeholders)-1]
			wheres = append(wheres, "p.address IN ("+placeholders+")")
			for _, a := range clean {
				args = append(args, a)
			}
		}
	}
	if search != "" {
		wheres = append(wheres, `(LOWER(p.address) LIKE ? OR LOWER(p.symbol0) LIKE ? OR LOWER(p.symbol1) LIKE ?)`)
		like := "%" + search + "%"
		args = append(args, like, like, like)
	}
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	// Secondary sort by address ensures stable ordering when many rows tie at 0.
	query += " ORDER BY " + sortBy + " DESC, p.address ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	pools := []Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.Address, &p.DEX, &p.Token0, &p.Token1, &p.Symbol0, &p.Symbol1,
			&p.FeeBps, &p.FeePPM, &p.TVLUSD, &p.Volume24hUSD, &p.SpotPrice,
			&p.IsStable, &p.Disabled, &p.IsDead, &p.FirstSeen, &p.LastUpdated); err != nil {
			continue
		}
		pools = append(pools, p)
	}

	// Total count uses the same UNION subquery so V4 pools are included.
	var total int
	countQuery := `SELECT COUNT(*) FROM (
		SELECT p.address, p.dex, COALESCE(t0.symbol,'') AS symbol0, COALESCE(t1.symbol,'') AS symbol1
		FROM pools p
		LEFT JOIN tokens t0 ON t0.address = p.token0
		LEFT JOIN tokens t1 ON t1.address = p.token1
		WHERE p.dex != 'UniV4'
		UNION ALL
		SELECT v.pool_id AS address, 'UniV4' AS dex, COALESCE(t0.symbol,'') AS symbol0, COALESCE(t1.symbol,'') AS symbol1
		FROM v4_pools v
		LEFT JOIN tokens t0 ON t0.address = v.token0
		LEFT JOIN tokens t1 ON t1.address = v.token1
	) p`
	if len(wheres) > 0 {
		countQuery += " WHERE " + strings.Join(wheres, " AND ")
	}
	countArgs := args[:len(args)-2] // strip limit/offset
	db.QueryRow(countQuery, countArgs...).Scan(&total)

	// DEX list union — include UniV4 from v4_pools.
	dexRows, _ := db.Query(`SELECT dex FROM (
		SELECT DISTINCT dex FROM pools WHERE dex != 'UniV4'
		UNION SELECT 'UniV4' WHERE EXISTS(SELECT 1 FROM v4_pools)
	) ORDER BY dex`)
	var dexList []string
	if dexRows != nil {
		for dexRows.Next() {
			var d string
			if dexRows.Scan(&d) == nil {
				dexList = append(dexList, d)
			}
		}
		dexRows.Close()
	}

	jsonOK(w, map[string]interface{}{
		"pools":     pools,
		"total":     total,
		"dex_list":  dexList,
	})
}

// ─── /api/health ─────────────────────────────────────────────────────────────

// ─── /api/contracts ──────────────────────────────────────────────────────────
//
// Returns every row from the contract_ledger table: all deployed/planned
// contracts the bot knows about, their purpose (executor / flash_source /
// router / infra / oracle), which trade types and DEXes they support, and
// their gas estimates. Used by the dashboard's Contracts tab.

func apiContracts(db *sql.DB, w http.ResponseWriter, _ *http.Request) {
	type Contract struct {
		Address         string `json:"address"`
		Name            string `json:"name"`
		Kind            string `json:"kind"`
		TradeTypes      string `json:"trade_types"`
		SupportedDexes  string `json:"supported_dexes"`
		FlashSources    string `json:"flash_sources"`
		MaxHops         int    `json:"max_hops"`
		Deployer        string `json:"deployer"`
		DeployTx        string `json:"deploy_tx"`
		DeployBlock     int64  `json:"deploy_block"`
		ChainID         int64  `json:"chain_id"`
		Status          string `json:"status"`
		Notes           string `json:"notes"`
		CreatedAt       int64  `json:"created_at"`
		UpdatedAt       int64  `json:"updated_at"`
		GasEstimate     int64  `json:"gas_estimate"`
	}
	rows, err := db.Query(`SELECT address, name, kind, trade_types, supported_dexes,
	        flash_sources, max_hops, deployer, deploy_tx, deploy_block, chain_id,
	        status, notes, created_at, updated_at, gas_estimate
	        FROM contract_ledger
	        ORDER BY CASE kind WHEN 'executor' THEN 1 WHEN 'flash_source' THEN 2 WHEN 'router' THEN 3 WHEN 'oracle' THEN 4 ELSE 5 END,
	                 CASE status WHEN 'active' THEN 1 WHEN 'not_deployed' THEN 2 WHEN 'disabled' THEN 3 WHEN 'deprecated' THEN 4 ELSE 5 END,
	                 name ASC`)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	out := []Contract{}
	for rows.Next() {
		var c Contract
		if err := rows.Scan(&c.Address, &c.Name, &c.Kind, &c.TradeTypes, &c.SupportedDexes,
			&c.FlashSources, &c.MaxHops, &c.Deployer, &c.DeployTx, &c.DeployBlock, &c.ChainID,
			&c.Status, &c.Notes, &c.CreatedAt, &c.UpdatedAt, &c.GasEstimate); err != nil {
			continue
		}
		out = append(out, c)
	}
	jsonOK(w, out)
}

func apiHealth(db *sql.DB, w http.ResponseWriter) {
	// TrackedPoolStatus is one entry per V3/V4 pool the bot is currently tracking
	// for tick data. UpdatedAt is the unix seconds timestamp of the last
	// successful FetchTickMaps pass that touched it (0 if never updated).
	// HasTicks=1 means the Ticks slice was non-empty after that pass.
	// The dashboard computes the live age at render time as (now - updated_at).
	type TrackedPoolStatus struct {
		Addr      string `json:"address"`
		UpdatedAt int64  `json:"updated_at"`
		HasTicks  int    `json:"has_ticks"`
	}
	type HealthStatus struct {
		UpdatedAt         int64               `json:"updated_at"`
		TickDataAt        int64               `json:"tick_data_at"`
		MulticallAt       int64               `json:"multicall_at"`
		SwapEventAt       int64               `json:"swap_event_at"`
		CycleRebuildAt    int64               `json:"cycle_rebuild_at"`
		TickedPoolsHave   int64               `json:"ticked_pools_have"`
		TickedPoolsTotal  int64               `json:"ticked_pools_total"`
		TrackedPoolAddrs  []string            `json:"tracked_pool_addrs"`
		TrackedPoolStatus []TrackedPoolStatus `json:"tracked_pool_status"`
		// RPCState passes through whatever the bot wrote — see rpcHealthLoop in
		// internal/bot.go for the schema. We don't redeclare the struct here so
		// the bot can add fields without dashboard rebuilds.
		RPCState json.RawMessage `json:"rpc_state"`
	}
	var h HealthStatus
	var rawAddrs string
	var rawRPC sql.NullString
	err := db.QueryRow(`SELECT updated_at, tick_data_at, multicall_at, swap_event_at, cycle_rebuild_at, ticked_pools_have, ticked_pools_total, tracked_pool_addrs, rpc_state FROM bot_health WHERE id=1`).
		Scan(&h.UpdatedAt, &h.TickDataAt, &h.MulticallAt, &h.SwapEventAt, &h.CycleRebuildAt, &h.TickedPoolsHave, &h.TickedPoolsTotal, &rawAddrs, &rawRPC)
	if err != nil {
		jsonOK(w, map[string]interface{}{"error": "no health data yet"})
		return
	}
	// The bot stores a compact JSON array of {a,u,t} objects (address,
	// updated_at unix seconds, has_ticks). Parse and re-expose with friendlier
	// field names. Fall back to plain []string for older bot versions that
	// wrote bare addresses.
	if rawAddrs != "" {
		var compact []struct {
			A string `json:"a"`
			U int64  `json:"u"`
			T int    `json:"t"`
		}
		if err := json.Unmarshal([]byte(rawAddrs), &compact); err == nil && len(compact) > 0 && compact[0].A != "" {
			for _, p := range compact {
				h.TrackedPoolAddrs = append(h.TrackedPoolAddrs, p.A)
				h.TrackedPoolStatus = append(h.TrackedPoolStatus, TrackedPoolStatus{
					Addr: p.A, UpdatedAt: p.U, HasTicks: p.T,
				})
			}
		} else {
			_ = json.Unmarshal([]byte(rawAddrs), &h.TrackedPoolAddrs)
		}
	}
	if h.TrackedPoolAddrs == nil {
		h.TrackedPoolAddrs = []string{}
	}
	if h.TrackedPoolStatus == nil {
		h.TrackedPoolStatus = []TrackedPoolStatus{}
	}
	if rawRPC.Valid && rawRPC.String != "" {
		h.RPCState = json.RawMessage(rawRPC.String)
	} else {
		h.RPCState = json.RawMessage("[]")
	}
	jsonOK(w, h)
}

// ─── /api/trade (single trade detail + forensics) ────────────────────────────

func apiTradeDetail(db *sql.DB, w http.ResponseWriter, r *http.Request, addrToSym map[string]string) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonErr(w, "missing id", 400)
		return
	}
	row := db.QueryRow(`SELECT
		t.id, t.submitted_at, t.tx_hash, t.status, t.block_number, t.block_position, t.block_total_txs,
		t.gas_used, t.hops, t.log_profit, t.profit_usd_est, t.dexes, t.tokens, t.pools, t.executor,
		COALESCE(cl.name, ''),
		t.detect_rpc, t.submit_rpc, t.competitors_before, t.competitor_hashes,
		t.hop_prices_json, t.amount_in_wei, t.sim_profit_wei, t.sim_profit_bps,
		t.gas_price_gwei, t.min_profit_usd_at_decision, t.pool_states_json,
		t.revert_reason, t.hop_forensics_json, t.optimizations
	FROM our_trades t
	LEFT JOIN contract_ledger cl ON LOWER(cl.address) = LOWER(t.executor)
	WHERE t.id=?`, id)

	type TradeDetail struct {
		ID                    int64    `json:"id"`
		SubmittedAt           int64    `json:"submitted_at"`
		TxHash                string   `json:"tx_hash"`
		Status                string   `json:"status"`
		BlockNumber           *int64   `json:"block_number"`
		BlockPosition         *int64   `json:"block_position"`
		BlockTotalTxs         *int64   `json:"block_total_txs"`
		GasUsed               *int64   `json:"gas_used"`
		Hops                  int      `json:"hops"`
		LogProfit             float64  `json:"log_profit"`
		ProfitUSDEst          float64  `json:"profit_usd_est"`
		Dexes                 string   `json:"dexes"`
		Tokens                string   `json:"tokens"`
		Pools                 string   `json:"pools"`
		Executor              string   `json:"executor"`
		ExecutorName          string   `json:"executor_name"`
		DetectRPC             string   `json:"detect_rpc"`
		SubmitRPC             string   `json:"submit_rpc"`
		CompetitorsBefore     *int     `json:"competitors_before"`
		CompetitorHashes      string   `json:"competitor_hashes"`
		HopPricesJSON         string   `json:"hop_prices_json"`
		AmountInWei           string   `json:"amount_in_wei"`
		SimProfitWei          string   `json:"sim_profit_wei"`
		SimProfitBps          int      `json:"sim_profit_bps"`
		GasPriceGwei          float64  `json:"gas_price_gwei"`
		MinProfitUSDDecision  float64  `json:"min_profit_usd_at_decision"`
		PoolStatesJSON        string   `json:"pool_states_json"`
		RevertReason          string   `json:"revert_reason"`
		HopForensicsJSON      string   `json:"hop_forensics_json"`
		Optimizations         string   `json:"optimizations"`
	}

	var t TradeDetail
	var rawPrices string
	err := row.Scan(
		&t.ID, &t.SubmittedAt, &t.TxHash, &t.Status, &t.BlockNumber, &t.BlockPosition, &t.BlockTotalTxs,
		&t.GasUsed, &t.Hops, &t.LogProfit, &t.ProfitUSDEst, &t.Dexes, &t.Tokens, &t.Pools, &t.Executor,
		&t.ExecutorName,
		&t.DetectRPC, &t.SubmitRPC, &t.CompetitorsBefore, &t.CompetitorHashes,
		&rawPrices, &t.AmountInWei, &t.SimProfitWei, &t.SimProfitBps,
		&t.GasPriceGwei, &t.MinProfitUSDDecision, &t.PoolStatesJSON,
		&t.RevertReason, &t.HopForensicsJSON, &t.Optimizations,
	)
	if err != nil {
		jsonErr(w, "trade not found: "+err.Error(), 404)
		return
	}
	t.HopPricesJSON = resolveHopPrices(rawPrices, addrToSym)
	jsonOK(w, t)
}

// ─── /api/query ──────────────────────────────────────────────────────────────

var blockedKeywords = []string{"drop ", "alter ", "create ", "attach ", "detach ", "vacuum", "pragma", "truncate"}

func apiQuery(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "POST required", 405)
		return
	}
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "invalid JSON", 400)
		return
	}
	q := strings.TrimSpace(body.SQL)
	if q == "" {
		jsonErr(w, "empty query", 400)
		return
	}
	lower := strings.ToLower(q)
	for _, kw := range blockedKeywords {
		if strings.Contains(lower, kw) {
			jsonErr(w, "blocked keyword: "+strings.TrimSpace(kw), 403)
			return
		}
	}

	type Result struct {
		Columns      []string         `json:"columns"`
		Rows         [][]interface{}  `json:"rows"`
		RowsAffected int64            `json:"rows_affected"`
		Error        string           `json:"error,omitempty"`
	}

	// Detect if it's a SELECT-like query or a mutating one
	isSelect := strings.HasPrefix(lower, "select") || strings.HasPrefix(lower, "with")

	res := Result{Columns: []string{}, Rows: [][]interface{}{}}

	if isSelect {
		rows, err := db.Query(q)
		if err != nil {
			jsonOK(w, Result{Error: err.Error()})
			return
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		res.Columns = cols
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			// Convert []byte to string for JSON readability
			row := make([]interface{}, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					row[i] = string(b)
				} else {
					row[i] = v
				}
			}
			res.Rows = append(res.Rows, row)
		}
	} else {
		result, err := db.Exec(q)
		if err != nil {
			jsonOK(w, Result{Error: err.Error()})
			return
		}
		res.RowsAffected, _ = result.RowsAffected()
	}

	jsonOK(w, res)
}
