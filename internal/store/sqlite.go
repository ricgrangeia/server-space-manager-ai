package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Sample struct {
	Kind     string // "host_path", "fs", "container_log", "volume", "image", "bind_mount"
	Key      string // path or id
	Label    string // human-friendly (container name, stack, etc.)
	Bytes    int64
	Extra    string // optional JSON
	TakenAt  time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS samples (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	kind     TEXT NOT NULL,
	key      TEXT NOT NULL,
	label    TEXT,
	bytes    INTEGER NOT NULL,
	extra    TEXT,
	taken_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_kind_key_time ON samples(kind, key, taken_at);
CREATE INDEX IF NOT EXISTS idx_samples_taken_at      ON samples(taken_at);

CREATE TABLE IF NOT EXISTS findings (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	kind      TEXT NOT NULL,         -- sample kind (container_log, volume, ...)
	key       TEXT NOT NULL,         -- sample key the finding refers to
	severity  TEXT NOT NULL,         -- info | warn | crit
	reason    TEXT NOT NULL,         -- model-generated one-liner
	source    TEXT NOT NULL,         -- 'rules' | 'ai_review' | 'digest'
	created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_findings_created_at ON findings(created_at);
CREATE INDEX IF NOT EXISTS idx_findings_key        ON findings(kind, key);
`)
	return err
}

// Finding mirrors aireview.Finding but lives in the store layer to avoid an
// import cycle. Persisted so /api/ask can reference past decisions.
type Finding struct {
	Kind      string
	Key       string
	Severity  string
	Reason    string
	Source    string
	CreatedAt time.Time
}

// InsertFindings persists a batch of findings.
func (s *Store) InsertFindings(fs []Finding) error {
	if len(fs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO findings(kind,key,severity,reason,source,created_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, f := range fs {
		if _, err := stmt.Exec(f.Kind, f.Key, f.Severity, f.Reason, f.Source, f.CreatedAt); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// RecentFindings returns the most recent N findings since `since`.
func (s *Store) RecentFindings(since time.Time, limit int) ([]Finding, error) {
	rows, err := s.db.Query(`
SELECT kind,key,severity,reason,source,created_at FROM findings
WHERE created_at >= ?
ORDER BY created_at DESC
LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.Kind, &f.Key, &f.Severity, &f.Reason, &f.Source, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Insert(samples []Sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO samples(kind,key,label,bytes,extra,taken_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, x := range samples {
		if _, err := stmt.Exec(x.Kind, x.Key, x.Label, x.Bytes, x.Extra, x.TakenAt); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// LatestScan returns every sample from the most recent scan (i.e. all rows
// whose taken_at equals the max taken_at in the table). Used at startup to
// restore the dashboard's in-memory snapshot without having to wait for the
// next scan to complete.
func (s *Store) LatestScan() ([]Sample, time.Time, error) {
	var t time.Time
	if err := s.db.QueryRow(`SELECT MAX(taken_at) FROM samples`).Scan(&t); err != nil {
		// No rows yet — fresh database.
		return nil, time.Time{}, nil
	}
	if t.IsZero() {
		return nil, time.Time{}, nil
	}
	rows, err := s.db.Query(`
SELECT kind,key,label,bytes,extra,taken_at FROM samples
WHERE taken_at = ?`, t)
	if err != nil {
		return nil, t, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.Kind, &x.Key, &x.Label, &x.Bytes, &x.Extra, &x.TakenAt); err != nil {
			return nil, t, err
		}
		out = append(out, x)
	}
	return out, t, rows.Err()
}

// Latest returns the most recent sample per (kind,key) for a given kind.
func (s *Store) Latest(kind string, limit int) ([]Sample, error) {
	rows, err := s.db.Query(`
SELECT kind,key,label,bytes,extra,taken_at FROM samples
WHERE kind = ? AND taken_at = (
	SELECT MAX(taken_at) FROM samples s2 WHERE s2.kind = samples.kind AND s2.key = samples.key
)
ORDER BY bytes DESC
LIMIT ?`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.Kind, &x.Key, &x.Label, &x.Bytes, &x.Extra, &x.TakenAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GrowthSince returns bytes delta per key since `since`.
func (s *Store) GrowthSince(kind string, since time.Time, limit int) ([]GrowthRow, error) {
	rows, err := s.db.Query(`
WITH first AS (
	SELECT key, bytes FROM samples
	WHERE kind = ? AND taken_at = (
		SELECT MIN(taken_at) FROM samples s2 WHERE s2.kind = samples.kind AND s2.key = samples.key AND s2.taken_at >= ?
	) AND taken_at >= ?
),
last AS (
	SELECT key, label, bytes, taken_at FROM samples
	WHERE kind = ? AND taken_at = (
		SELECT MAX(taken_at) FROM samples s2 WHERE s2.kind = samples.kind AND s2.key = samples.key
	)
)
SELECT l.key, l.label, l.bytes, (l.bytes - f.bytes) AS delta
FROM last l JOIN first f ON l.key = f.key
ORDER BY delta DESC
LIMIT ?`, kind, since, since, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrowthRow
	for rows.Next() {
		var g GrowthRow
		if err := rows.Scan(&g.Key, &g.Label, &g.Bytes, &g.DeltaBytes); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type GrowthRow struct {
	Key        string
	Label      string
	Bytes      int64
	DeltaBytes int64
}

// AnomalyRow describes an item whose most-recent-day growth deviates from
// its own historical daily average over a baseline window. A "ratio" above
// ~3 typically indicates the item just spiked relative to its norm.
type AnomalyRow struct {
	Kind          string
	Key           string
	Label         string
	Bytes         int64
	Last24hDelta  int64   // bytes added in the last 24h
	BaselineDaily float64 // average bytes/day over the baseline window
	Ratio         float64 // Last24hDelta / BaselineDaily (Inf if baseline == 0 and spike > 0)
}

// BaselineAnomalies finds items whose last-24h growth is significantly larger
// than their own historical daily average over the prior `baselineDays` days.
//
// The baseline window deliberately excludes the last 24h so a spiking item
// can't inflate its own baseline. Items with no usable history are skipped.
// `minRatio` filters out boring noise; pass ~3.0 to mean "growing >=3x normal."
// Items below `minDeltaBytes` are also dropped (avoids alerting on a 200-byte
// log file that "tripled").
func (s *Store) BaselineAnomalies(kind string, baselineDays int, minRatio float64, minDeltaBytes int64, limit int) ([]AnomalyRow, error) {
	now := time.Now().UTC()
	last24Start := now.Add(-24 * time.Hour)
	baseStart := last24Start.AddDate(0, 0, -baselineDays)

	// Per (kind,key): pull
	//   - latest bytes & latest taken_at
	//   - bytes at the start of the last-24h window
	//   - bytes at the start of the baseline window
	// Daily baseline = (bytes_at_last24_start - bytes_at_base_start) / baselineDays
	// 24h delta      = (latest_bytes        - bytes_at_last24_start)
	rows, err := s.db.Query(`
WITH params AS (SELECT ? AS kind, ? AS base_start, ? AS last24_start),
ranked AS (
    SELECT s.key, s.label, s.bytes, s.taken_at,
           ROW_NUMBER() OVER (PARTITION BY s.key ORDER BY s.taken_at DESC) AS rn_latest,
           ROW_NUMBER() OVER (PARTITION BY s.key ORDER BY ABS(strftime('%s', s.taken_at) - strftime('%s', (SELECT last24_start FROM params)))) AS rn_l24,
           ROW_NUMBER() OVER (PARTITION BY s.key ORDER BY ABS(strftime('%s', s.taken_at) - strftime('%s', (SELECT base_start    FROM params)))) AS rn_base
    FROM samples s
    WHERE s.kind = (SELECT kind FROM params)
      AND s.taken_at >= (SELECT base_start FROM params)
),
latest  AS (SELECT key, label, bytes AS latest_bytes  FROM ranked WHERE rn_latest = 1),
l24     AS (SELECT key,        bytes AS l24_bytes     FROM ranked WHERE rn_l24    = 1),
base    AS (SELECT key,        bytes AS base_bytes    FROM ranked WHERE rn_base   = 1)
SELECT l.key, l.label, l.latest_bytes,
       (l.latest_bytes - x.l24_bytes)                                AS delta_24h,
       CAST(MAX(x.l24_bytes - b.base_bytes, 0) AS REAL) / ?           AS baseline_daily
FROM latest l
JOIN l24  x ON x.key = l.key
JOIN base b ON b.key = l.key
`, kind, baseStart, last24Start, float64(baselineDays))
	if err != nil {
		return nil, fmt.Errorf("baseline anomalies: %w", err)
	}
	defer rows.Close()

	var out []AnomalyRow
	for rows.Next() {
		var r AnomalyRow
		r.Kind = kind
		if err := rows.Scan(&r.Key, &r.Label, &r.Bytes, &r.Last24hDelta, &r.BaselineDaily); err != nil {
			return nil, err
		}
		if r.Last24hDelta < minDeltaBytes {
			continue
		}
		// Compute ratio. A zero baseline with a positive spike is meaningful
		// (brand-new growth), so report it with a large sentinel ratio.
		if r.BaselineDaily <= 0 {
			r.Ratio = 999
		} else {
			r.Ratio = float64(r.Last24hDelta) / r.BaselineDaily
		}
		if r.Ratio < minRatio {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sort by ratio desc, cap at limit.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Ratio > out[j-1].Ratio; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Prune removes samples and findings older than olderThan. Returns the total
// number of rows deleted across both tables.
func (s *Store) Prune(olderThan time.Time) (int64, error) {
	r1, err := s.db.Exec(`DELETE FROM samples  WHERE taken_at < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune samples: %w", err)
	}
	r2, err := s.db.Exec(`DELETE FROM findings WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune findings: %w", err)
	}
	a, _ := r1.RowsAffected()
	b, _ := r2.RowsAffected()
	return a + b, nil
}
