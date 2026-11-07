package tesla

import (
	"log"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// LogEntry is a single charging decision record.
type LogEntry struct {
	Timestamp  string `db:"ts"          json:"ts"`
	Source     string `db:"source"      json:"source"` // "user" or "auto"
	Mode       string `db:"mode"        json:"mode"`
	Status     string `db:"status"      json:"status"`
	Amps       int    `db:"amps"        json:"amps"`
	AvailableW int    `db:"available_w" json:"available_w"`
	PowerW     int    `db:"power_w"     json:"power_w"`
	Reason     string `db:"reason"      json:"reason"`
}

// BLELogEntry is a single BLE command execution record.
type BLELogEntry struct {
	Timestamp string `db:"ts"      json:"ts"`
	Command   string `db:"command" json:"command"`
	Success   bool   `db:"success" json:"success"`
	Error     string `db:"error"   json:"error"`
}

// Stats holds permanent cumulative statistics.
type Stats struct {
	TotalSolarKWh float64 `db:"total_solar_kwh" json:"total_solar_kwh"`
}

// Store handles SQLite persistence for charging logs and stats.
type Store struct {
	db         *sqlx.DB
	retainDays int
}

// NewStore opens (or creates) the SQLite database at the given path.
// retainDays controls how many days of log entries to keep.
func NewStore(dbPath string, retainDays int) (*Store, error) {
	db, err := sqlx.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}

	// Create tables if they don't exist.
	db.MustExec(`
		CREATE TABLE IF NOT EXISTS charge_log (
			ts          DATETIME DEFAULT (datetime('now')),
			source      TEXT NOT NULL DEFAULT 'auto',
			mode        TEXT NOT NULL,
			status      TEXT NOT NULL,
			amps        INTEGER NOT NULL,
			available_w INTEGER NOT NULL,
			power_w     INTEGER NOT NULL,
			reason      TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS ble_command_log (
			ts      DATETIME DEFAULT (datetime('now')),
			command TEXT NOT NULL,
			success INTEGER NOT NULL,
			error   TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_ble_command_log_ts ON ble_command_log(ts);

		CREATE TABLE IF NOT EXISTS charge_stats (
			id              INTEGER PRIMARY KEY CHECK (id = 1),
			total_solar_kwh REAL NOT NULL DEFAULT 0
		);

		-- Ensure the single stats row exists.
		INSERT OR IGNORE INTO charge_stats (id, total_solar_kwh) VALUES (1, 0);

		-- Index for efficient pruning and time-range queries.
		CREATE INDEX IF NOT EXISTS idx_charge_log_ts ON charge_log(ts);
	`)

	// Migrate: add source column if missing (existing installs)
	db.Exec(`ALTER TABLE charge_log ADD COLUMN source TEXT NOT NULL DEFAULT 'auto'`)

	s := &Store{db: db, retainDays: retainDays}
	s.prune() // clean up old entries on startup
	return s, nil
}

// LogDecision writes a charging decision to the log.
// source is "user" for manual mode changes or "auto" for system decisions.
func (s *Store) LogDecision(source, mode, status string, amps, availableW, powerW int, reason string) {
	s.LogDecisionAt(time.Now(), source, mode, status, amps, availableW, powerW, reason)
}

// LogDecisionAt is LogDecision with an explicit time (the demo seeds a history).
func (s *Store) LogDecisionAt(ts time.Time, source, mode, status string, amps, availableW, powerW int, reason string) {
	_, err := s.db.Exec(
		`INSERT INTO charge_log (ts, source, mode, status, amps, available_w, power_w, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.Format("2006-01-02 15:04:05"),
		source, mode, status, amps, availableW, powerW, reason,
	)
	if err != nil {
		log.Printf("[STORE] Log error: %v", err)
	}
}

// LogBLECommand records a BLE command execution result.
func (s *Store) LogBLECommand(command string, err error) {
	success := 1
	errMsg := ""
	if err != nil {
		success = 0
		errMsg = err.Error()
	}
	_, dbErr := s.db.Exec(
		`INSERT INTO ble_command_log (ts, command, success, error) VALUES (?, ?, ?, ?)`,
		time.Now().Format("2006-01-02 15:04:05"),
		command, success, errMsg,
	)
	if dbErr != nil {
		log.Printf("[STORE] BLE log error: %v", dbErr)
	}
}

// GetBLELog returns recent BLE command log entries, newest first.
func (s *Store) GetBLELog(limit int) []BLELogEntry {
	var entries []BLELogEntry
	q := `SELECT ts, command, success, error FROM ble_command_log ORDER BY ts DESC`
	if limit > 0 {
		q += " LIMIT ?"
		if err := s.db.Select(&entries, q, limit); err != nil {
			log.Printf("[STORE] BLE log read error: %v", err)
		}
	} else {
		if err := s.db.Select(&entries, q); err != nil {
			log.Printf("[STORE] BLE log read error: %v", err)
		}
	}
	return entries
}

// AddSolarEnergy adds the given kWh to the cumulative solar charging total.
func (s *Store) AddSolarEnergy(kwh float64) {
	if kwh <= 0 {
		return
	}
	_, err := s.db.Exec(
		`UPDATE charge_stats SET total_solar_kwh = total_solar_kwh + ? WHERE id = 1`,
		kwh,
	)
	if err != nil {
		log.Printf("[STORE] Stats update error: %v", err)
	}
}

// GetStats returns the cumulative charging statistics.
func (s *Store) GetStats() Stats {
	var stats Stats
	err := s.db.Get(&stats, `SELECT total_solar_kwh FROM charge_stats WHERE id = 1`)
	if err != nil {
		log.Printf("[STORE] Stats read error: %v", err)
	}
	return stats
}

// GetLog returns recent log entries, newest first.
// limit controls the max number of entries returned (0 = all within retention).
func (s *Store) GetLog(limit int) []LogEntry {
	var entries []LogEntry
	q := `SELECT ts, source, mode, status, amps, available_w, power_w, reason
	      FROM charge_log ORDER BY ts DESC`
	if limit > 0 {
		q += " LIMIT ?"
		if err := s.db.Select(&entries, q, limit); err != nil {
			log.Printf("[STORE] Log read error: %v", err)
		}
	} else {
		if err := s.db.Select(&entries, q); err != nil {
			log.Printf("[STORE] Log read error: %v", err)
		}
	}
	return entries
}

// prune deletes log entries older than retainDays.
func (s *Store) prune() {
	cutoff := time.Now().AddDate(0, 0, -s.retainDays).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(`DELETE FROM charge_log WHERE ts < ?`, cutoff)
	if err != nil {
		log.Printf("[STORE] Prune error: %v", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("[STORE] Pruned %d old charge log entries", n)
	}
	result, err = s.db.Exec(`DELETE FROM ble_command_log WHERE ts < ?`, cutoff)
	if err != nil {
		log.Printf("[STORE] BLE prune error: %v", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("[STORE] Pruned %d old BLE log entries", n)
	}
}

// PrunePeriodic runs prune every 6 hours in the background.
func (s *Store) PrunePeriodic() {
	go func() {
		for {
			time.Sleep(6 * time.Hour)
			s.prune()
		}
	}()
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
