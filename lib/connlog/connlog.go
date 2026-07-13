// Package connlog persists TCP tunnel connection records (real client
// address, resolved geo, tunnel) to a local SQLite database so history
// survives restarts and is never pruned automatically -- only an explicit
// Delete call removes rows.
package connlog

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ehang.io/nps/lib/geoip"
	"ehang.io/nps/lib/translate"
	_ "modernc.org/sqlite"
)

var db *sql.DB

const timeLayout = "2006-01-02 15:04:05"

// Record is one stored connection.
type Record struct {
	ID         int64
	TunnelId   int
	Remark     string
	Port       int
	RemoteAddr string
	Time       time.Time
	Country    string
	Province   string
	City       string
	ISP        string
	Lat        float64
	Lng        float64
	HasGeo     bool
	// Domain is the HTTP Host header or TLS SNI hostname sniffed from the
	// connection, if any (empty for non-HTTP/TLS traffic).
	Domain string
}

// Init opens (creating if needed) the SQLite database at dbPath and ensures
// the schema exists. Safe to call once at startup.
func Init(dbPath string) error {
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	// SQLite only supports one writer at a time; serialize through a single
	// connection rather than juggling SQLITE_BUSY retries.
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS conn_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tunnel_id INTEGER NOT NULL,
			remark TEXT,
			port INTEGER,
			remote_addr TEXT NOT NULL,
			time TEXT NOT NULL,
			country TEXT,
			province TEXT,
			city TEXT,
			isp TEXT,
			lat REAL,
			lng REAL,
			has_geo INTEGER
		);
		CREATE INDEX IF NOT EXISTS idx_conn_logs_time ON conn_logs(time DESC);
		CREATE INDEX IF NOT EXISTS idx_conn_logs_tunnel ON conn_logs(tunnel_id);
		CREATE INDEX IF NOT EXISTS idx_conn_logs_country ON conn_logs(country);
	`); err != nil {
		conn.Close()
		return err
	}
	// migrate: add columns introduced after the initial schema. SQLite has
	// no "ADD COLUMN IF NOT EXISTS", so just ignore the duplicate-column
	// error on databases that already have them.
	for _, stmt := range []string{
		`ALTER TABLE conn_logs ADD COLUMN domain TEXT`,
	} {
		if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			conn.Close()
			return err
		}
	}
	db = conn
	return nil
}

// Insert stores one connection record. r.Time is set to now if zero.
func Insert(r *Record) error {
	if db == nil {
		return fmt.Errorf("connlog: not initialized")
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	_, err := db.Exec(
		`INSERT INTO conn_logs (tunnel_id, remark, port, remote_addr, time, country, province, city, isp, lat, lng, has_geo, domain)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TunnelId, r.Remark, r.Port, r.RemoteAddr, r.Time.Format(timeLayout),
		r.Country, r.Province, r.City, r.ISP, r.Lat, r.Lng, boolToInt(r.HasGeo), r.Domain,
	)
	return err
}

// Filter narrows List results. Zero values mean "no filter" for that field.
type Filter struct {
	Search   string // matches remote_addr, country, province, city, remark, domain (substring)
	TunnelId int
}

func (f Filter) where() (string, []interface{}) {
	var clauses []string
	var args []interface{}
	if f.Search != "" {
		clauses = append(clauses, "(remote_addr LIKE ? OR country LIKE ? OR province LIKE ? OR city LIKE ? OR remark LIKE ? OR domain LIKE ?)")
		like := "%" + f.Search + "%"
		args = append(args, like, like, like, like, like, like)
	}
	if f.TunnelId != 0 {
		clauses = append(clauses, "tunnel_id = ?")
		args = append(args, f.TunnelId)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// List returns a page of records (newest first) matching filter, plus the
// total matching row count (for pagination).
func List(offset, limit int, filter Filter) ([]*Record, int, error) {
	if db == nil {
		return nil, 0, fmt.Errorf("connlog: not initialized")
	}
	whereSQL, args := filter.where()

	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM conn_logs"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.Query(
		"SELECT id, tunnel_id, remark, port, remote_addr, time, country, province, city, isp, lat, lng, has_geo, COALESCE(domain, '') FROM conn_logs"+
			whereSQL+" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(append([]interface{}{}, args...), limit, offset)...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	list := make([]*Record, 0, limit)
	for rows.Next() {
		r := &Record{}
		var timeStr string
		var hasGeo int
		if err := rows.Scan(&r.ID, &r.TunnelId, &r.Remark, &r.Port, &r.RemoteAddr, &timeStr,
			&r.Country, &r.Province, &r.City, &r.ISP, &r.Lat, &r.Lng, &hasGeo, &r.Domain); err != nil {
			return nil, 0, err
		}
		r.Time, _ = time.ParseInLocation(timeLayout, timeStr, time.Local)
		r.HasGeo = hasGeo != 0
		// normalize on read too so records stored before the ISP cleanup
		// (e.g. Tencent Cloud's street-address ISP string) display cleanly
		r.ISP = geoip.NormalizeISP(r.ISP)
		// Use a cached Chinese translation for display if one has been
		// resolved (translate.Enqueue is called when the record is first
		// inserted); this never blocks on the translation API itself.
		if provinceZH, cityZH, ok := translate.Lookup(r.Country, r.Province, r.City); ok {
			if provinceZH != "" {
				r.Province = provinceZH
			}
			if cityZH != "" {
				r.City = cityZH
			}
		}
		list = append(list, r)
	}
	return list, total, rows.Err()
}

// Recent returns the most recent limit records across all tunnels, used to
// plot dots on the connection map.
func Recent(limit int) ([]*Record, error) {
	list, _, err := List(0, limit, Filter{})
	return list, err
}

// Delete removes the given record ids. Records are otherwise never pruned.
func Delete(ids []int64) error {
	if db == nil {
		return fmt.Errorf("connlog: not initialized")
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := db.Exec("DELETE FROM conn_logs WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	return err
}

// CountryCount is one row of the country ranking.
type CountryCount struct {
	Country string
	Count   int
}

// CountByCountry returns the top countries by connection count, descending.
func CountByCountry(limit int) ([]CountryCount, error) {
	if db == nil {
		return nil, fmt.Errorf("connlog: not initialized")
	}
	rows, err := db.Query(
		`SELECT country, COUNT(*) c FROM conn_logs WHERE country != '' GROUP BY country ORDER BY c DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// non-nil so json.Marshal produces [] instead of null when there are no
	// rows yet (e.g. a fresh install with no geo-resolved connections)
	out := make([]CountryCount, 0)
	for rows.Next() {
		var cc CountryCount
		if err := rows.Scan(&cc.Country, &cc.Count); err != nil {
			return nil, err
		}
		out = append(out, cc)
	}
	return out, rows.Err()
}

// IPCount is one row of the client IP ranking.
type IPCount struct {
	IP    string
	Count int
}

// ipExpr extracts the host part of remote_addr ("ip:port" or "[ipv6]:port"),
// stripping the port so connections from the same client group together.
const ipExpr = `CASE WHEN substr(remote_addr, 1, 1) = '[' ` +
	`THEN substr(remote_addr, 1, instr(remote_addr, ']')) ` +
	`ELSE substr(remote_addr, 1, instr(remote_addr, ':') - 1) END`

// CountByIP returns the top client IPs by connection count, descending.
func CountByIP(limit int) ([]IPCount, error) {
	if db == nil {
		return nil, fmt.Errorf("connlog: not initialized")
	}
	rows, err := db.Query(
		`SELECT `+ipExpr+` ip, COUNT(*) c FROM conn_logs WHERE remote_addr != '' GROUP BY ip ORDER BY c DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IPCount, 0)
	for rows.Next() {
		var ic IPCount
		if err := rows.Scan(&ic.IP, &ic.Count); err != nil {
			return nil, err
		}
		out = append(out, ic)
	}
	return out, rows.Err()
}

// DistinctIPCount returns the number of distinct client IPs seen.
func DistinctIPCount() (int, error) {
	if db == nil {
		return 0, fmt.Errorf("connlog: not initialized")
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(DISTINCT ` + ipExpr + `) FROM conn_logs WHERE remote_addr != ''`).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
