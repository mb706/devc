// Access state in the hub database (work order C11b): the user's password
// and TOTP secret (meta), login sessions bound to a device certificate,
// failed logins per certificate, enrolment tokens, action tokens of
// notification buttons, the notifications outstanding, and when each
// certificate was last seen. Tokens are stored as their SHA-256 only.

package hub

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const accessSchema = `
CREATE TABLE IF NOT EXISTS cert_seen (fingerprint TEXT PRIMARY KEY, last_seen TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS logins (
	id TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL,
	device TEXT NOT NULL,
	created TEXT NOT NULL,
	last_used TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS logins_fp ON logins (fingerprint);
CREATE TABLE IF NOT EXISTS login_failures (
	fingerprint TEXT PRIMARY KEY,
	count INTEGER NOT NULL,
	last TEXT NOT NULL,
	locked_until TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS enrolments (
	id TEXT PRIMARY KEY,
	role TEXT NOT NULL,
	name TEXT NOT NULL,
	created TEXT NOT NULL,
	expires TEXT NOT NULL,
	used TEXT NOT NULL DEFAULT '',
	bundle BLOB,
	cert TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS actions (
	id TEXT PRIMARY KEY,
	machine TEXT NOT NULL,
	session TEXT NOT NULL,
	question TEXT NOT NULL,
	choice INTEGER NOT NULL,
	label TEXT NOT NULL,
	command TEXT NOT NULL,
	seq TEXT NOT NULL,
	expires TEXT NOT NULL,
	used TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS actions_seq ON actions (seq);
CREATE TABLE IF NOT EXISTS notifications (
	machine TEXT NOT NULL,
	session TEXT NOT NULL,
	key TEXT NOT NULL,
	seq TEXT NOT NULL,
	sent TEXT NOT NULL,
	PRIMARY KEY (machine, session)
);
CREATE TABLE IF NOT EXISTS notify_states (
	machine TEXT NOT NULL,
	session TEXT NOT NULL,
	key TEXT NOT NULL,
	state TEXT NOT NULL,
	seq TEXT NOT NULL,
	tries INTEGER NOT NULL DEFAULT 0,
	next_at TEXT NOT NULL DEFAULT '',
	created TEXT NOT NULL,
	PRIMARY KEY (machine, session, key)
);
`

// Meta keys of the user.
const (
	metaPassword     = "user_password"
	metaTOTPSecret   = "user_totp_secret"
	metaTOTPLastStep = "user_totp_last_step"
)

// Lifetimes and limits of access state.
const (
	// SessionIdle is how long a login session lasts without use.
	SessionIdle = 30 * 24 * time.Hour
	// LoginFailures failed logins in a row lock a certificate's logins for
	// LoginLock.
	LoginFailures = 5
	LoginLock     = 15 * time.Minute
	// EnrolTTL is the longest an enrolment token is valid.
	EnrolTTL = 15 * time.Minute
	// ActionTTL is how long a notification button works.
	ActionTTL = time.Hour
)

// --- The user ---

// SetPassword stores a password hash.
func (s *Store) SetPassword(hash string) error { return s.SetMeta(metaPassword, hash) }

// PasswordHash returns the stored hash ("" when none is set).
func (s *Store) PasswordHash() (string, error) { return s.Meta(metaPassword) }

// SetTOTP stores (or, with "", removes) the TOTP secret.
func (s *Store) SetTOTP(secret string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM meta WHERE key IN (?, ?)`, metaTOTPSecret, metaTOTPLastStep); err != nil {
		return err
	}
	if secret != "" {
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, metaTOTPSecret, secret); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TOTP returns the TOTP secret ("" when TOTP is off) and the last step a
// code was accepted for.
func (s *Store) TOTP() (string, int64, error) {
	secret, err := s.Meta(metaTOTPSecret)
	if err != nil {
		return "", 0, err
	}
	v, err := s.Meta(metaTOTPLastStep)
	if err != nil {
		return "", 0, err
	}
	last, _ := strconv.ParseInt(v, 10, 64)
	return secret, last, nil
}

// UseTOTPStep records that a code of step was used, unless a later step was
// recorded meanwhile; it reports whether step was new.
func (s *Store) UseTOTPStep(step int64) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var v string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaTOTPLastStep).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if last, _ := strconv.ParseInt(v, 10, 64); err == nil && step <= last {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaTOTPLastStep, strconv.FormatInt(step, 10)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// --- Certificates last seen ---

// SeeCert records that a certificate was used.
func (s *Store) SeeCert(fingerprint string, now time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO cert_seen (fingerprint, last_seen) VALUES (?, ?) ON CONFLICT (fingerprint) DO UPDATE SET last_seen = excluded.last_seen`, fingerprint, ts(now))
	return err
}

// CertsSeen returns when each certificate was last used.
func (s *Store) CertsSeen() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT fingerprint, last_seen FROM cert_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var fp, at string
		if err := rows.Scan(&fp, &at); err != nil {
			return nil, err
		}
		out[fp] = parseTS(at)
	}
	return out, rows.Err()
}

// --- Login sessions ---

// Login is a login session.
type Login struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Device      string    `json:"device"`
	Created     time.Time `json:"created"`
	LastUsed    time.Time `json:"last_used"`
}

// Expires is when the session ends without further use.
func (l *Login) Expires() time.Time { return l.LastUsed.Add(SessionIdle) }

// AddLogin stores a new session.
func (s *Store) AddLogin(l Login) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO logins (id, fingerprint, device, created, last_used) VALUES (?, ?, ?, ?, ?)`, l.ID, l.Fingerprint, l.Device, ts(l.Created), ts(l.LastUsed))
	return err
}

const loginCols = `id, fingerprint, device, created, last_used`

func scanLogin(sc interface{ Scan(...any) error }) (Login, error) {
	var l Login
	var created, used string
	if err := sc.Scan(&l.ID, &l.Fingerprint, &l.Device, &created, &used); err != nil {
		return l, err
	}
	l.Created, l.LastUsed = parseTS(created), parseTS(used)
	return l, nil
}

// LoginByID returns a session.
func (s *Store) LoginByID(id string) (Login, bool, error) {
	l, err := scanLogin(s.db.QueryRow(`SELECT `+loginCols+` FROM logins WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// TouchLogin records the use of a session.
func (s *Store) TouchLogin(id string, now time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE logins SET last_used = ? WHERE id = ?`, ts(now), id)
	return err
}

// Logins lists the sessions, oldest first.
func (s *Store) Logins() ([]Login, error) {
	rows, err := s.db.Query(`SELECT ` + loginCols + ` FROM logins ORDER BY created, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Login{}
	for rows.Next() {
		l, err := scanLogin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLogins removes sessions: by ID prefix (at least 8 hex characters),
// by device name, or all of them ("*"). It returns how many it removed.
func (s *Store) DeleteLogins(match string) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var res sql.Result
	var err error
	switch {
	case match == "*":
		res, err = s.db.Exec(`DELETE FROM logins`)
	case len(match) >= 8 && isHex(match):
		res, err = s.db.Exec(`DELETE FROM logins WHERE substr(id, 1, ?) = ? OR device = ?`, len(match), strings.ToLower(match), match)
	default:
		res, err = s.db.Exec(`DELETE FROM logins WHERE device = ?`, match)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteLogin removes one session by ID.
func (s *Store) DeleteLogin(id string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM logins WHERE id = ?`, id)
	return err
}

// PruneLogins removes sessions unused since before cutoff.
func (s *Store) PruneLogins(cutoff time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM logins WHERE last_used < ?`, ts(cutoff))
	return err
}

func isHex(s string) bool {
	for _, c := range strings.ToLower(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// --- Failed logins ---

// LoginLocked reports until when a certificate's logins are locked (zero
// when they are not).
func (s *Store) LoginLocked(fingerprint string, now time.Time) (time.Time, error) {
	var until string
	err := s.db.QueryRow(`SELECT locked_until FROM login_failures WHERE fingerprint = ?`, fingerprint).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if t := parseTS(until); t.After(now) {
		return t, nil
	}
	return time.Time{}, nil
}

// LoginFailed counts a failed login of a certificate and returns until when
// its logins are now locked (zero when they are not). Failures older than
// LoginLock, and those before a lock that has ended, are forgotten.
func (s *Store) LoginFailed(fingerprint string, now time.Time) (time.Time, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var count int
	var last, until string
	err = tx.QueryRow(`SELECT count, last, locked_until FROM login_failures WHERE fingerprint = ?`, fingerprint).Scan(&count, &last, &until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	lock := parseTS(until)
	if lock.After(now) {
		return lock, nil
	}
	if !lock.IsZero() || now.Sub(parseTS(last)) > LoginLock {
		count, lock = 0, time.Time{}
	}
	count++
	if count >= LoginFailures {
		lock = now.Add(LoginLock)
	}
	if _, err := tx.Exec(`INSERT INTO login_failures (fingerprint, count, last, locked_until) VALUES (?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET count = excluded.count, last = excluded.last, locked_until = excluded.locked_until`,
		fingerprint, count, ts(now), ts(lock)); err != nil {
		return time.Time{}, err
	}
	return lock, tx.Commit()
}

// LoginSucceeded forgets a certificate's failed logins.
func (s *Store) LoginSucceeded(fingerprint string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM login_failures WHERE fingerprint = ?`, fingerprint)
	return err
}

// --- Enrolments ---

// Enrolment is an enrolment token.
type Enrolment struct {
	ID      string
	Role    string
	Name    string
	Created time.Time
	Expires time.Time
	Used    time.Time
	// Bundle and CertPEM are a device's PKCS#12 bundle and certificate,
	// made when the token was created and registered when downloaded.
	Bundle  []byte
	CertPEM string
}

// AddEnrolment stores a new token.
func (s *Store) AddEnrolment(e Enrolment) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO enrolments (id, role, name, created, expires, bundle, cert) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Role, e.Name, ts(e.Created), ts(e.Expires), e.Bundle, e.CertPEM)
	return err
}

// EnrolmentOpen reports whether any enrolment token is outstanding (unused
// and not expired).
func (s *Store) EnrolmentOpen(now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM enrolments WHERE used = '' AND expires > ?`, ts(now)).Scan(&n)
	return n > 0, err
}

// Enrolments lists the outstanding tokens.
func (s *Store) Enrolments(now time.Time) ([]Enrolment, error) {
	rows, err := s.db.Query(`SELECT id, role, name, created, expires FROM enrolments WHERE used = '' AND expires > ? ORDER BY created`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Enrolment
	for rows.Next() {
		var e Enrolment
		var c, x string
		if err := rows.Scan(&e.ID, &e.Role, &e.Name, &c, &x); err != nil {
			return nil, err
		}
		e.Created, e.Expires = parseTS(c), parseTS(x)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnrolmentUsable reports whether a token of role is outstanding.
func (s *Store) EnrolmentUsable(id, role string, now time.Time) (bool, error) {
	var gotID, gotRole, x, used string
	err := s.db.QueryRow(`SELECT id, role, expires, used FROM enrolments WHERE id = ?`, id).Scan(&gotID, &gotRole, &x, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameID(gotID, id) && gotRole == role && used == "" && now.Before(parseTS(x)), nil
}

// errTokenUnusable is returned for an unknown, used or expired token.
var errTokenUnusable = errors.New("the token is unknown, used or expired")

// UseEnrolment takes an outstanding token of role for use: it is marked
// used in the same transaction as complete (which registers what the
// enrolment produced), so a token works once even with two concurrent
// requests, and is not used up when complete fails.
func (s *Store) UseEnrolment(id, role string, now time.Time, complete func(e Enrolment, tx *sql.Tx) error) (Enrolment, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Enrolment{}, err
	}
	defer tx.Rollback()
	var e Enrolment
	var c, x, used string
	err = tx.QueryRow(`SELECT id, role, name, created, expires, used, bundle, cert FROM enrolments WHERE id = ?`, id).Scan(&e.ID, &e.Role, &e.Name, &c, &x, &used, &e.Bundle, &e.CertPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrolment{}, errTokenUnusable
	}
	if err != nil {
		return Enrolment{}, err
	}
	e.Created, e.Expires = parseTS(c), parseTS(x)
	if !sameID(e.ID, id) || e.Role != role || used != "" || !now.Before(e.Expires) {
		return Enrolment{}, errTokenUnusable
	}
	res, err := tx.Exec(`UPDATE enrolments SET used = ?, bundle = NULL, cert = '' WHERE id = ? AND used = ''`, ts(now), id)
	if err != nil {
		return Enrolment{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Enrolment{}, errTokenUnusable
	}
	if err := complete(e, tx); err != nil {
		return Enrolment{}, err
	}
	e.Used = now
	return e, tx.Commit()
}

// PruneEnrolments removes tokens that expired a day ago (with any bundle
// they still hold).
func (s *Store) PruneEnrolments(now time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM enrolments WHERE expires < ?`, ts(now.Add(-24*time.Hour)))
	if err == nil {
		_, err = s.db.Exec(`UPDATE enrolments SET bundle = NULL, cert = '' WHERE expires < ?`, ts(now))
	}
	return err
}

// addCertTx records an issued certificate inside a transaction.
func addCertTx(tx *sql.Tx, c CertRecord) error {
	if _, err := tx.Exec(`INSERT INTO certs (fingerprint, role, name, serial, issued, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Fingerprint, c.Role, c.Name, c.Serial, ts(c.Issued), ts(c.Expires)); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, '1') ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, metaRegistryGen)
	return err
}

// --- Action tokens ---

// Action is the one command a notification button performs.
type Action struct {
	ID       string
	Machine  string
	Session  string
	Question string
	Choice   int
	Label    string
	// Command is the command ID the answer is sent with, so a retry is
	// applied once by the machine.
	Command string
	// Seq is the sequence ID of the notification that carries the button.
	Seq     string
	Expires time.Time
}

// AddActions stores action tokens.
func (s *Store) AddActions(as []Action) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range as {
		if _, err := tx.Exec(`INSERT INTO actions (id, machine, session, question, choice, label, command, seq, expires) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.Machine, a.Session, a.Question, a.Choice, a.Label, a.Command, a.Seq, ts(a.Expires)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseAction takes an action token: it must be unused and not expired. The
// other buttons of the same notification are used up with it (a question
// is answered once).
func (s *Store) UseAction(id string, now time.Time) (Action, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Action{}, err
	}
	defer tx.Rollback()
	var a Action
	var x, used string
	err = tx.QueryRow(`SELECT id, machine, session, question, choice, label, command, seq, expires, used FROM actions WHERE id = ?`, id).
		Scan(&a.ID, &a.Machine, &a.Session, &a.Question, &a.Choice, &a.Label, &a.Command, &a.Seq, &x, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, errTokenUnusable
	}
	if err != nil {
		return Action{}, err
	}
	a.Expires = parseTS(x)
	if !sameID(a.ID, id) || used != "" || !now.Before(a.Expires) {
		return Action{}, errTokenUnusable
	}
	if _, err := tx.Exec(`UPDATE actions SET used = ? WHERE seq = ? AND used = ''`, ts(now), a.Seq); err != nil {
		return Action{}, err
	}
	return a, tx.Commit()
}

// DeleteActions removes the tokens of a notification.
func (s *Store) DeleteActions(seq string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM actions WHERE seq = ?`, seq)
	return err
}

// PruneActions removes expired tokens.
func (s *Store) PruneActions(now time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM actions WHERE expires < ?`, ts(now))
	return err
}

// --- Notification states ---

// Notification states (findings C2-13, C2-14): a record per attention
// state of a session (its notifyKey) says what became of its notification.
const (
	// NotePending: to be published, by this hub once it is the machine's
	// notifier, while the state lasts (publishing failures are retried).
	NotePending = "pending"
	// NoteSent: published (Seq names the message), or handled without a
	// message (Seq empty: a client followed the session, or it was not
	// news).
	NoteSent = "sent"
	// NoteCleared: the state ended after its message was published; the
	// message is cleared at ntfy (retried until ntfy confirms, then the
	// record goes).
	NoteCleared = "cleared"
)

// NoteState is the notification record of one attention state.
type NoteState struct {
	Machine, Session, Key string
	State                 string
	// Seq is the ntfy sequence ID, chosen when the record is made, so a
	// retried publish replaces rather than repeats the message.
	Seq     string
	Tries   int
	NextAt  time.Time
	Created time.Time
}

// migrateNotifications turns the records of the C11b notifications table
// (published messages) into notification states.
func migrateNotifications(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO notify_states (machine, session, key, state, seq, tries, next_at, created)
		SELECT machine, session, key, 'sent', seq, 0, '', sent FROM notifications`); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM notifications`)
	return err
}

// NoteStates returns every notification record.
func (s *Store) NoteStates() ([]NoteState, error) {
	rows, err := s.db.Query(`SELECT machine, session, key, state, seq, tries, next_at, created FROM notify_states`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteState
	for rows.Next() {
		var n NoteState
		var next, created string
		if err := rows.Scan(&n.Machine, &n.Session, &n.Key, &n.State, &n.Seq, &n.Tries, &next, &created); err != nil {
			return nil, err
		}
		n.NextAt, n.Created = parseTS(next), parseTS(created)
		out = append(out, n)
	}
	return out, rows.Err()
}

// PutNoteState stores a notification record.
func (s *Store) PutNoteState(n NoteState) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO notify_states (machine, session, key, state, seq, tries, next_at, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (machine, session, key) DO UPDATE SET state = excluded.state, seq = excluded.seq, tries = excluded.tries, next_at = excluded.next_at`,
		n.Machine, n.Session, n.Key, n.State, n.Seq, n.Tries, ts(n.NextAt), ts(n.Created))
	return err
}

// DeleteNoteState removes a notification record.
func (s *Store) DeleteNoteState(machine, session, key string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM notify_states WHERE machine = ? AND session = ? AND key = ?`, machine, session, key)
	return err
}

// PeekAction returns an action token that is still usable, without using
// it.
func (s *Store) PeekAction(id string, now time.Time) (Action, bool, error) {
	var a Action
	var x, used string
	err := s.db.QueryRow(`SELECT id, machine, session, question, choice, label, command, seq, expires, used FROM actions WHERE id = ?`, id).
		Scan(&a.ID, &a.Machine, &a.Session, &a.Question, &a.Choice, &a.Label, &a.Command, &a.Seq, &x, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	a.Expires = parseTS(x)
	return a, sameID(a.ID, id) && used == "" && now.Before(a.Expires), nil
}
