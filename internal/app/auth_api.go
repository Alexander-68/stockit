package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"stockit/internal/auth"
)

func (s *Server) principalFromRequest(r *http.Request) (Principal, bool) {
	session, ok := s.sessionFromRequest(r)
	if !ok {
		return Principal{}, false
	}
	return principalFromSession(session), true
}

func principalFromSession(session *auth.Session) Principal {
	return Principal{
		UserID:    session.UserID,
		LoginName: session.LoginName,
		Role:      session.Role,
		Token:     session.Token,
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

func (s *Server) authenticateCredentials(ctx context.Context, loginName, password string) (*auth.Session, error) {
	user, err := s.store.AuthenticateUser(ctx, strings.TrimSpace(loginName))
	if err != nil {
		return nil, errors.New("invalid login credentials")
	}

	ok, err := auth.VerifyPassword(user.PasswordHash, password)
	if err != nil || !ok {
		return nil, errors.New("invalid login credentials")
	}

	session, err := s.sessions.Create(user.ID, user.LoginName, user.Role)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Server) handleAPIAuthLogin(w http.ResponseWriter, r *http.Request) {
	var payload apiLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "invalid JSON body"})
		return
	}

	if strings.TrimSpace(payload.LoginName) == "" || strings.TrimSpace(payload.Password) == "" {
		s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "login_name and password are required"})
		return
	}

	session, err := s.authenticateCredentials(r.Context(), payload.LoginName, payload.Password)
	if errors.Is(err, auth.ErrSessionLimit) {
		s.writeJSON(w, http.StatusForbidden, apiErrorResponse{Error: "session limit reached"})
		return
	}
	if err != nil {
		s.writeJSON(w, http.StatusUnauthorized, apiErrorResponse{Error: err.Error()})
		return
	}

	s.setSessionCookie(w, session.Token, s.isHTTPS(r))
	s.writeJSON(w, http.StatusOK, apiLoginResponse{
		Token:                  session.Token,
		TokenType:              "Bearer",
		User:                   session.LoginName,
		Role:                   session.Role,
		SessionIdleTimeoutSecs: int64((15 * time.Minute) / time.Second),
	})
}

func (s *Server) handleAPIAuthLogout(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	s.sessions.Delete(principal.Token)
	s.clearSessionCookie(w, s.isHTTPS(r))
	w.WriteHeader(http.StatusNoContent)
}
