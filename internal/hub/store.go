package hub

// The hub's SQLite database (pure Go, modernc.org/sqlite): the certificate
// registry, the machines, the latest view per machine and session with the
// cursor it was folded to, a bounded tail of events per session, pending
// requests, and a bounded change log that server-sent event streams resume
// from. Writes are serialised in this process; other processes (devc hub ca
// revoke) wait on SQLite's lock.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mb706/devc/internal/proto"
)

// Bounds of the projection; variables so tests can shrink them.
var (
	// TailEvents and TailBytes bound the events kept per session.
	TailEvents = 500
	TailBytes  = 1 << 20
	// KeepChanges bounds the change log streams resume from.
	KeepChanges = 20000
)

const schemaVersion = "1"

// Meta keys.
const (
	metaSchema      = "schema"
	metaPublicName  = "public_name"
	metaRegistryGen = "registry_gen"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS certs (
	fingerprint TEXT PRIMARY KEY,
	role TEXT NOT NULL,
	name TEXT NOT NULL,
	serial TEXT NOT NULL,
	issued TEXT NOT NULL,
	expires TEXT NOT NULL,
	revoked TEXT
);
CREATE INDEX IF NOT EXISTS certs_name ON certs (name);
CREATE TABLE IF NOT EXISTS machines (
	name TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	instance TEXT NOT NULL DEFAULT '',
	online INTEGER NOT NULL DEFAULT 0,
	notifier INTEGER NOT NULL DEFAULT 0,
	connected TEXT NOT NULL DEFAULT '',
	last_seen TEXT NOT NULL DEFAULT '',
	boxes TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS sessions (
	machine TEXT NOT NULL,
	session TEXT NOT NULL,
	view TEXT NOT NULL,
	last_seq INTEGER NOT NULL,
	PRIMARY KEY (machine, session)
);
CREATE TABLE IF NOT EXISTS events (
	machine TEXT NOT NULL,
	session TEXT NOT NULL,
	seq INTEGER NOT NULL,
	size INTEGER NOT NULL,
	event TEXT NOT NULL,
	PRIMARY KEY (machine, session, seq)
);
CREATE TABLE IF NOT EXISTS requests (
	machine TEXT NOT NULL,
	id TEXT NOT NULL,
	session TEXT NOT NULL DEFAULT '',
	request TEXT NOT NULL,
	PRIMARY KEY (machine, id)
);
CREATE TABLE IF NOT EXISTS changes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	machine TEXT NOT NULL DEFAULT '',
	session TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL
);
`

// Store is the hub database.
type Store struct {
	db  *sql.DB
	wmu sync.Mutex

	mu      sync.Mutex
	changed chan struct{}
}

// OpenStore opens (and creates) the database at path.
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db, changed: make(chan struct{})}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening the hub database %s: %w", path, err)
	}
	v, err := s.Meta(metaSchema)
	switch {
	case err != nil:
		db.Close()
		return nil, err
	case v == "":
		if err := s.SetMeta(metaSchema, schemaVersion); err != nil {
			db.Close()
			return nil, err
		}
	case v != schemaVersion:
		db.Close()
		return nil, fmt.Errorf("the hub database %s has schema %s; this devc knows %s", path, v, schemaVersion)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Meta returns a meta value ("" when unset).
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta sets a meta value.
func (s *Store) SetMeta(key, value string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// --- Registry ---

// CertRecord is one issued certificate.
type CertRecord struct {
	Fingerprint string     `json:"fingerprint"`
	Role        string     `json:"role"`
	Name        string     `json:"name"`
	Serial      string     `json:"serial"`
	Issued      time.Time  `json:"issued"`
	Expires     time.Time  `json:"expires"`
	Revoked     *time.Time `json:"revoked,omitempty"`
}

// Valid reports whether the certificate may be used at now.
func (c *CertRecord) Valid(now time.Time) bool {
	return c.Revoked == nil && now.Before(c.Expires) && (c.Role == RoleMachine || c.Role == RoleDevice)
}

func (s *Store) bumpRegistry(tx *sql.Tx) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, '1') ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, metaRegistryGen)
	return err
}

// AddCert records an issued certificate.
func (s *Store) AddCert(c CertRecord) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO certs (fingerprint, role, name, serial, issued, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Fingerprint, c.Role, c.Name, c.Serial, ts(c.Issued), ts(c.Expires)); err != nil {
		return err
	}
	if err := s.bumpRegistry(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func scanCert(sc interface{ Scan(...any) error }) (CertRecord, error) {
	var c CertRecord
	var issued, expires string
	var revoked sql.NullString
	if err := sc.Scan(&c.Fingerprint, &c.Role, &c.Name, &c.Serial, &issued, &expires, &revoked); err != nil {
		return c, err
	}
	c.Issued, c.Expires = parseTS(issued), parseTS(expires)
	if revoked.Valid {
		t := parseTS(revoked.String)
		c.Revoked = &t
	}
	return c, nil
}

const certCols = `fingerprint, role, name, serial, issued, expires, revoked`

// Cert returns the record of a fingerprint.
func (s *Store) Cert(fingerprint string) (CertRecord, bool, error) {
	c, err := scanCert(s.db.QueryRow(`SELECT `+certCols+` FROM certs WHERE fingerprint = ?`, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// Certs lists the registry by issue time.
func (s *Store) Certs() ([]CertRecord, error) {
	rows, err := s.db.Query(`SELECT ` + certCols + ` FROM certs ORDER BY issued, fingerprint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CertRecord
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Revoke revokes every unrevoked certificate whose name is match (or
// whose fingerprint starts with match, at least 8 hex characters), limited
// to role when given. It returns the revoked records.
func (s *Store) Revoke(match, role string) ([]CertRecord, error) {
	all, err := s.Certs()
	if err != nil {
		return nil, err
	}
	var hit []CertRecord
	for _, c := range all {
		if c.Revoked != nil || (role != "" && c.Role != role) {
			continue
		}
		if c.Name == match || (len(match) >= 8 && strings.HasPrefix(c.Fingerprint, strings.ToLower(match))) {
			hit = append(hit, c)
		}
	}
	if len(hit) == 0 {
		return nil, nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for i := range hit {
		if _, err := tx.Exec(`UPDATE certs SET revoked = ? WHERE fingerprint = ?`, ts(now), hit[i].Fingerprint); err != nil {
			return nil, err
		}
		hit[i].Revoked = &now
	}
	if err := s.bumpRegistry(tx); err != nil {
		return nil, err
	}
	return hit, tx.Commit()
}

// RegistryGen changes whenever the registry changes.
func (s *Store) RegistryGen() (string, error) { return s.Meta(metaRegistryGen) }

// --- Changes ---

// Change is one entry of the change log.
type Change struct {
	ID      int64
	Kind    string
	Machine string
	Session string
	Data    json.RawMessage
}

// Changed returns a channel closed at the next committed change.
func (s *Store) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Store) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// ChangesAfter returns up to limit changes with IDs above id.
func (s *Store) ChangesAfter(id int64, limit int) ([]Change, error) {
	rows, err := s.db.Query(`SELECT id, kind, machine, session, data FROM changes WHERE id > ? ORDER BY id LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var data string
		if err := rows.Scan(&c.ID, &c.Kind, &c.Machine, &c.Session, &data); err != nil {
			return nil, err
		}
		c.Data = json.RawMessage(data)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangeRange returns the lowest and highest change IDs kept (0, 0 when
// none) and the highest ever assigned.
func (s *Store) ChangeRange() (lo, hi int64, err error) {
	var l, h sql.NullInt64
	if err = s.db.QueryRow(`SELECT MIN(id), MAX(id) FROM changes`).Scan(&l, &h); err != nil {
		return 0, 0, err
	}
	var seq sql.NullInt64
	_ = s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'changes'`).Scan(&seq)
	return l.Int64, max(h.Int64, seq.Int64), nil
}

// --- Writes ---

// Tx is one write transaction.
type Tx struct {
	tx      *sql.Tx
	changes int
}

// Write runs fn in a transaction; streams are woken after a commit that
// added changes.
func (s *Store) Write(fn func(*Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	sqlTx, err := s.db.Begin()
	if err != nil {
		return err
	}
	t := &Tx{tx: sqlTx}
	if err := fn(t); err != nil {
		sqlTx.Rollback()
		return err
	}
	if t.changes > 0 {
		if _, err := sqlTx.Exec(`DELETE FROM changes WHERE id <= (SELECT MAX(id) FROM changes) - ?`, KeepChanges); err != nil {
			sqlTx.Rollback()
			return err
		}
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	if t.changes > 0 {
		s.signal()
	}
	return nil
}

// AddChange appends to the change log.
func (t *Tx) AddChange(kind, machine, session string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(`INSERT INTO changes (kind, machine, session, data) VALUES (?, ?, ?, ?)`, kind, machine, session, string(b)); err != nil {
		return err
	}
	t.changes++
	return nil
}

// PutEvent stores one event of a session (ignored when present).
func (t *Tx) PutEvent(machine, session string, ev proto.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT OR IGNORE INTO events (machine, session, seq, size, event) VALUES (?, ?, ?, ?, ?)`, machine, session, ev.Seq, len(b), string(b))
	return err
}

// TrimTail keeps the most recent TailEvents events of a session and at
// most TailBytes of them.
func (t *Tx) TrimTail(machine, session string) error {
	rows, err := t.tx.Query(`SELECT seq, size FROM events WHERE machine = ? AND session = ? ORDER BY seq DESC`, machine, session)
	if err != nil {
		return err
	}
	var cut int64 = -1
	n, total := 0, 0
	for rows.Next() {
		var seq int64
		var size int
		if err := rows.Scan(&seq, &size); err != nil {
			rows.Close()
			return err
		}
		n++
		total += size
		if n > TailEvents || (total > TailBytes && n > 1) {
			cut = seq
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if cut < 0 {
		return nil
	}
	_, err = t.tx.Exec(`DELETE FROM events WHERE machine = ? AND session = ? AND seq <= ?`, machine, session, cut)
	return err
}

// PutSession stores a session's view and the seq it was folded to.
func (t *Tx) PutSession(machine string, v proto.SessionView, lastSeq int64) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT INTO sessions (machine, session, view, last_seq) VALUES (?, ?, ?, ?)
		ON CONFLICT (machine, session) DO UPDATE SET view = excluded.view, last_seq = excluded.last_seq`, machine, v.Session, string(b), lastSeq)
	return err
}

// DeleteSession removes a session and its events.
func (t *Tx) DeleteSession(machine, session string) error {
	if _, err := t.tx.Exec(`DELETE FROM events WHERE machine = ? AND session = ?`, machine, session); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM sessions WHERE machine = ? AND session = ?`, machine, session)
	return err
}

// DeleteEvents removes the stored events of a session.
func (t *Tx) DeleteEvents(machine, session string) error {
	_, err := t.tx.Exec(`DELETE FROM events WHERE machine = ? AND session = ?`, machine, session)
	return err
}

// PutRequest stores a pending request.
func (t *Tx) PutRequest(machine string, r proto.Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT INTO requests (machine, id, session, request) VALUES (?, ?, ?, ?)
		ON CONFLICT (machine, id) DO UPDATE SET session = excluded.session, request = excluded.request`, machine, r.ID, r.Session, string(b))
	return err
}

// DeleteRequest removes a request.
func (t *Tx) DeleteRequest(machine, id string) error {
	_, err := t.tx.Exec(`DELETE FROM requests WHERE machine = ? AND id = ?`, machine, id)
	return err
}

// MachineRow is a machine as stored.
type MachineRow struct {
	Name        string          `json:"name"`
	Fingerprint string          `json:"-"`
	Host        string          `json:"host,omitempty"`
	Version     string          `json:"version,omitempty"`
	Instance    string          `json:"instance,omitempty"`
	Online      bool            `json:"online"`
	Notifier    bool            `json:"notifier"`
	Connected   *time.Time      `json:"connected,omitempty"`
	LastSeen    *time.Time      `json:"last_seen,omitempty"`
	Boxes       json.RawMessage `json:"boxes"`
}

// MachineUp records a machine that connected and said hello.
func (t *Tx) MachineUp(name, fingerprint string, h proto.HubHello, now time.Time) error {
	_, err := t.tx.Exec(`INSERT INTO machines (name, fingerprint, host, version, instance, online, notifier, connected, last_seen)
		VALUES (?, ?, ?, ?, ?, 1, 0, ?, ?)
		ON CONFLICT (name) DO UPDATE SET fingerprint = excluded.fingerprint, host = excluded.host, version = excluded.version,
			instance = excluded.instance, online = 1, notifier = 0, connected = excluded.connected, last_seen = excluded.last_seen`,
		name, fingerprint, h.Name, h.Version, h.Instance, ts(now), ts(now))
	return err
}

// MachineDown marks a machine offline (and no longer notifying here).
func (t *Tx) MachineDown(name string, now time.Time) error {
	_, err := t.tx.Exec(`UPDATE machines SET online = 0, notifier = 0, last_seen = ? WHERE name = ?`, ts(now), name)
	return err
}

// SetNotifier records a machine's hub.role.
func (t *Tx) SetNotifier(name string, on bool) error {
	_, err := t.tx.Exec(`UPDATE machines SET notifier = ? WHERE name = ?`, on, name)
	return err
}

// SetBoxes stores a machine's box list.
func (t *Tx) SetBoxes(name string, boxes []proto.BoxView) error {
	b, err := json.Marshal(boxes)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`UPDATE machines SET boxes = ? WHERE name = ?`, string(b), name)
	return err
}

// Touch records that a machine was heard from.
func (s *Store) Touch(name string, now time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE machines SET last_seen = ? WHERE name = ?`, ts(now), name)
	return err
}

// ResetOnline marks every machine offline (at start: nothing is connected).
func (s *Store) ResetOnline() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE machines SET online = 0, notifier = 0`)
	return err
}

// --- Reads ---

const machineCols = `name, fingerprint, host, version, instance, online, notifier, connected, last_seen, boxes`

func scanMachine(sc interface{ Scan(...any) error }) (MachineRow, error) {
	var m MachineRow
	var connected, lastSeen, boxes string
	if err := sc.Scan(&m.Name, &m.Fingerprint, &m.Host, &m.Version, &m.Instance, &m.Online, &m.Notifier, &connected, &lastSeen, &boxes); err != nil {
		return m, err
	}
	if t := parseTS(connected); !t.IsZero() {
		m.Connected = &t
	}
	if t := parseTS(lastSeen); !t.IsZero() {
		m.LastSeen = &t
	}
	m.Boxes = json.RawMessage(boxes)
	return m, nil
}

// Machines lists the machines by name.
func (s *Store) Machines() ([]MachineRow, error) {
	rows, err := s.db.Query(`SELECT ` + machineCols + ` FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MachineRow{}
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Machine returns one machine.
func (s *Store) Machine(name string) (MachineRow, bool, error) {
	m, err := scanMachine(s.db.QueryRow(`SELECT `+machineCols+` FROM machines WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// SessionRow is a stored session with its machine's online state.
type SessionRow struct {
	Machine string
	View    proto.SessionView
	LastSeq int64
	Online  bool
}

// SessionFilter selects sessions.
type SessionFilter struct {
	Machine, Realm, Project, Session string
	// Live: nil for any, else only live (true) or ended (false) sessions.
	Live *bool
}

// Sessions returns the matching sessions.
func (s *Store) Sessions(f SessionFilter) ([]SessionRow, error) {
	q := `SELECT s.machine, s.view, s.last_seq, COALESCE(m.online, 0) FROM sessions s LEFT JOIN machines m ON m.name = s.machine WHERE 1 = 1`
	var args []any
	if f.Machine != "" {
		q += ` AND s.machine = ?`
		args = append(args, f.Machine)
	}
	if f.Session != "" {
		q += ` AND s.session = ?`
		args = append(args, f.Session)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var view string
		if err := rows.Scan(&r.Machine, &view, &r.LastSeq, &r.Online); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(view), &r.View) != nil {
			continue
		}
		if (f.Realm != "" && r.View.Realm != f.Realm) || (f.Project != "" && r.View.Project != f.Project) || (f.Live != nil && r.View.Live != *f.Live) {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Cursor returns the seq each session of a machine was folded to.
func (s *Store) Cursor(machine string) (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT session, last_seq FROM sessions WHERE machine = ?`, machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, err
		}
		out[id] = seq
	}
	return out, rows.Err()
}

// Events returns up to limit of a session's stored events with seq below
// before (0: no bound), most recent ones, in seq order.
func (s *Store) Events(machine, session string, before int64, limit int) ([]proto.Event, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.db.Query(`SELECT event FROM events WHERE machine = ? AND session = ? AND seq < ? ORDER BY seq DESC LIMIT ?`, machine, session, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.Event
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var ev proto.Event
		if json.Unmarshal([]byte(b), &ev) == nil {
			out = append(out, ev)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// Requests returns the stored (pending) requests, of one machine when
// machine is given.
func (s *Store) Requests(machine string) ([]RequestRow, error) {
	q := `SELECT r.machine, r.request, COALESCE(m.online, 0) FROM requests r LEFT JOIN machines m ON m.name = r.machine`
	var args []any
	if machine != "" {
		q += ` WHERE r.machine = ?`
		args = append(args, machine)
	}
	rows, err := s.db.Query(q+` ORDER BY r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestRow
	for rows.Next() {
		var r RequestRow
		var b string
		if err := rows.Scan(&r.Machine, &b, &r.Online); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(b), &r.Request) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// RequestRow is a stored request with its machine.
type RequestRow struct {
	Machine string
	Request proto.Request
	Online  bool
}

// PendingBySession counts pending requests per machine and session.
func (s *Store) PendingBySession() (map[[2]string]int, error) {
	rows, err := s.db.Query(`SELECT machine, session, COUNT(*) FROM requests WHERE session != '' GROUP BY machine, session`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]int{}
	for rows.Next() {
		var m, sid string
		var n int
		if err := rows.Scan(&m, &sid, &n); err != nil {
			return nil, err
		}
		out[[2]string{m, sid}] = n
	}
	return out, rows.Err()
}

// Prune removes sessions that ended before cutoff, with their events.
func (s *Store) Prune(cutoff time.Time) (int, error) {
	rows, err := s.Sessions(SessionFilter{})
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.Write(func(t *Tx) error {
		for _, r := range rows {
			if r.View.Live || r.View.LastEvent.After(cutoff) {
				continue
			}
			if err := t.DeleteSession(r.Machine, r.View.Session); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
