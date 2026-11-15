package hub

// Login (work order C11b section 1). The hub has one user: a password
// (argon2id) and optionally TOTP. POST /api/login with a device
// certificate creates a session: a 256-bit random token in a cookie
// (HttpOnly, Secure, SameSite=Strict, Path=/, __Host- prefix), stored as
// its hash, bound to the fingerprint of that certificate, valid for 30
// days of inactivity. Every other /api/ route but the notification buttons
// needs the certificate and a session bound to it (Server.checkSession, run
// by guard). Five failed logins in a row lock that certificate's logins for
// 15 minutes; every attempt is logged, never the password.

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "__Host-devc-session"

// loginTouch is how often a session's last use is written.
const loginTouch = time.Minute

type loginKey struct{}

// loginOf returns the session guard attached to a request.
func loginOf(r *http.Request) Login {
	l, _ := r.Context().Value(loginKey{}).(Login)
	return l
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// checkSession finds the request's login session and checks that it is
// bound to the request's certificate and still valid. On failure it has
// answered.
func (s *Server) checkSession(w http.ResponseWriter, r *http.Request, id Identity) (Login, bool) {
	if s.o.DevNoAuth {
		return s.devLogin, true
	}
	now := s.now()
	fail := func(why string) (Login, bool) {
		if s.limited(w, "c:"+id.Fingerprint) {
			return Login{}, false
		}
		s.lim.fail("c:"+id.Fingerprint, now)
		s.log.Debug("no valid login session", "cert", id.Name, "why", why)
		// The login screen asks for a code only when one is needed.
		secret, _, _ := s.st.TOTP()
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "log in first (POST /api/login)", "login": true, "totp": secret != ""})
		return Login{}, false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || !ValidToken(c.Value) {
		return fail("no session cookie")
	}
	lid := TokenID(c.Value)
	l, ok, err := s.st.LoginByID(lid)
	if err != nil {
		s.internal(w, err)
		return Login{}, false
	}
	switch {
	case !ok || !sameID(l.ID, lid):
		return fail("unknown session")
	case subtle.ConstantTimeCompare([]byte(l.Fingerprint), []byte(id.Fingerprint)) != 1:
		// A session cookie presented with another certificate was copied:
		// it ends here.
		s.log.Warn("a session cookie came with another device certificate; the session is ended", "cert", id.Name, "session_device", l.Device)
		if err := s.st.DeleteLogin(l.ID); err != nil {
			s.log.Error("ending a session", "err", err)
		}
		return fail("session of another certificate")
	case !now.Before(l.Expires()):
		_ = s.st.DeleteLogin(l.ID)
		return fail("session expired")
	}
	if now.Sub(l.LastUsed) >= loginTouch {
		if err := s.st.TouchLogin(l.ID, now); err != nil {
			s.log.Warn("recording a session's use", "err", err)
		} else {
			l.LastUsed = now
			// The cookie lives as long as the session would without use.
			s.setSessionCookie(w, c.Value, int(SessionIdle/time.Second))
		}
	}
	return l, true
}

// LoginRequest is the body of POST /api/login.
type LoginRequest struct {
	Password string `json:"password"`
	TOTP     string `json:"totp,omitempty"`
}

// LoginReply is the answer to a successful login and of GET /api/me.
type LoginReply struct {
	Device  string    `json:"device"`
	Session string    `json:"session"`
	Expires time.Time `json:"expires"`
	// TerminalControl says the hub lets the web app take control of
	// terminals (hub.toml [terminal] control); Dev marks the development
	// mode without authentication.
	TerminalControl bool `json:"terminal_control"`
	Dev             bool `json:"dev,omitempty"`
}

// loginSlot takes the certificate's one login slot; false when another
// attempt of that certificate is in progress. With one attempt at a time,
// the fifth failure locks the next attempt, however many are sent at once
// (finding C2-03).
func (s *Server) loginSlot(fp string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginBusy[fp] {
		return nil, false
	}
	s.loginBusy[fp] = true
	return func() {
		s.mu.Lock()
		delete(s.loginBusy, fp)
		s.mu.Unlock()
	}, true
}

// hashSlot admits a login to password hashing: at most two hash at a time
// (64 MiB each) and maxHashWaiting wait; beyond that the hub is busy.
func (s *Server) hashSlot(ctx context.Context) (release func(), ok bool) {
	select {
	case s.hashQueue <- struct{}{}:
	default:
		return nil, false
	}
	select {
	case s.hashSem <- struct{}{}:
	case <-ctx.Done():
		<-s.hashQueue
		return nil, false
	}
	return func() { <-s.hashSem; <-s.hashQueue }, true
}

// locked answers 429 when the certificate's logins are locked at now.
func (s *Server) locked(w http.ResponseWriter, fp string, now time.Time) (bool, error) {
	until, err := s.st.LoginLocked(fp, now)
	if err != nil || until.IsZero() {
		return false, err
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "too many failed logins with this device; try again after %s", until.UTC().Format(time.RFC3339))
	return true, nil
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	if s.o.DevNoAuth {
		var req LoginRequest
		if !decodeBody(w, r, &req) {
			return
		}
		writeJSON(w, http.StatusOK, LoginReply{Device: id.Name, Session: s.devLogin.ID[:12], Expires: s.devLogin.Expires(), TerminalControl: true, Dev: true})
		return
	}
	attempt := func(outcome string) { s.log.Info("login attempt", "cert", id.Name, "outcome", outcome) }
	if s.limited(w, "c:"+id.Fingerprint) {
		attempt("rate-limited")
		return
	}
	release, ok := s.loginSlot(id.Fingerprint)
	if !ok {
		attempt("another attempt in progress")
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "another login with this device is in progress")
		return
	}
	defer release()
	if lk, err := s.locked(w, id.Fingerprint, s.now()); err != nil {
		s.internal(w, err)
		return
	} else if lk {
		attempt("locked")
		return
	}
	var req LoginRequest
	if !decodeBody(w, r, &req) {
		attempt("malformed")
		return
	}
	hash, err := s.st.PasswordHash()
	if err != nil {
		s.internal(w, err)
		return
	}
	if hash == "" {
		attempt("no password set")
		writeError(w, http.StatusForbidden, "%s", errNoPassword.Error())
		return
	}
	done, ok := s.hashSlot(r.Context())
	if !ok {
		attempt("busy")
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "the hub is busy with other logins; try again")
		return
	}
	defer done()
	// The lock is decided again right before the password is looked at.
	now := s.now()
	if lk, err := s.locked(w, id.Fingerprint, now); err != nil {
		s.internal(w, err)
		return
	} else if lk {
		attempt("locked")
		return
	}
	secret, last, err := s.st.TOTP()
	if err != nil {
		s.internal(w, err)
		return
	}
	passOK := VerifyPassword(hash, req.Password)
	codeOK, outcome := true, "wrong password"
	if secret != "" {
		var step int64
		step, codeOK = CheckTOTP(secret, req.TOTP, now, last)
		if passOK && codeOK {
			// The step is taken in a transaction: two logins with the same
			// code cannot both succeed.
			if codeOK, err = s.st.UseTOTPStep(step); err != nil {
				s.internal(w, err)
				return
			}
		}
		if passOK && !codeOK {
			outcome = "wrong code"
		}
	}
	if !passOK || !codeOK {
		lock, err := s.st.LoginFailed(id.Fingerprint, now)
		if err != nil {
			s.internal(w, err)
			return
		}
		s.lim.fail("c:"+id.Fingerprint, now)
		if !lock.IsZero() {
			outcome += "; logins locked"
		}
		attempt(outcome)
		msg := "wrong password or code"
		if !lock.IsZero() {
			msg += "; logins with this device are locked until " + lock.UTC().Format(time.RFC3339)
		}
		writeError(w, http.StatusUnauthorized, "%s", msg)
		return
	}
	if err := s.st.LoginSucceeded(id.Fingerprint); err != nil {
		s.internal(w, err)
		return
	}
	token, lid, err := NewToken()
	if err != nil {
		s.internal(w, err)
		return
	}
	l := Login{ID: lid, Fingerprint: id.Fingerprint, Device: id.Name, Created: now, LastUsed: now}
	if err := s.st.AddLogin(l); err != nil {
		s.internal(w, err)
		return
	}
	attempt("ok")
	s.setSessionCookie(w, token, int(SessionIdle/time.Second))
	writeJSON(w, http.StatusOK, LoginReply{Device: id.Name, Session: lid[:12], Expires: l.Expires(), TerminalControl: s.cfg.Terminal.Control})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	l := loginOf(r)
	if s.o.DevNoAuth {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	if err := s.st.DeleteLogin(l.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("logout", "cert", l.Device, "session", l.ID[:12])
	s.setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	l := loginOf(r)
	writeJSON(w, http.StatusOK, LoginReply{Device: l.Device, Session: l.ID[:12], Expires: l.Expires(), TerminalControl: s.cfg.Terminal.Control, Dev: s.o.DevNoAuth})
}
