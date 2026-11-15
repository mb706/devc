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
	now := s.now()
	fail := func(why string) (Login, bool) {
		if s.limited(w, "c:"+id.Fingerprint) {
			return Login{}, false
		}
		s.lim.fail("c:"+id.Fingerprint, now)
		s.log.Debug("no valid login session", "cert", id.Name, "why", why)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "log in first (POST /api/login)", "login": true})
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
}

// verifyPassword hashes with at most two logins at a time (each takes 64
// MiB).
func (s *Server) verifyPassword(ctx context.Context, hash, password string) bool {
	select {
	case s.hashSem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.hashSem }()
	return VerifyPassword(hash, password)
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	now := s.now()
	attempt := func(outcome string) { s.log.Info("login attempt", "cert", id.Name, "outcome", outcome) }
	if s.limited(w, "c:"+id.Fingerprint) {
		attempt("rate-limited")
		return
	}
	until, err := s.st.LoginLocked(id.Fingerprint, now)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !until.IsZero() {
		attempt("locked")
		w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins with this device; try again after %s", until.UTC().Format(time.RFC3339))
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
	secret, last, err := s.st.TOTP()
	if err != nil {
		s.internal(w, err)
		return
	}
	passOK := s.verifyPassword(r.Context(), hash, req.Password)
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
	writeJSON(w, http.StatusOK, LoginReply{Device: id.Name, Session: lid[:12], Expires: l.Expires()})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	l := loginOf(r)
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
	writeJSON(w, http.StatusOK, LoginReply{Device: l.Device, Session: l.ID[:12], Expires: l.Expires()})
}
