package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

var ErrSessionLimit = errors.New("session limit reached")

type Session struct {
	Token      string
	UserID     int64
	LoginName  string
	Role       string
	LastActive time.Time
}

type Manager struct {
	mu          sync.Mutex
	sessions    map[string]*Session
	maxSessions int
	idleTimeout time.Duration
	now         func() time.Time
}

func NewManager(maxSessions int, idleTimeout time.Duration) *Manager {
	return &Manager{
		sessions:    make(map[string]*Session),
		maxSessions: maxSessions,
		idleTimeout: idleTimeout,
		now:         time.Now,
	}
}

func (m *Manager) Create(userID int64, loginName, role string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneLocked()
	if len(m.sessions) >= m.maxSessions {
		return nil, ErrSessionLimit
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	session := &Session{
		Token:      token,
		UserID:     userID,
		LoginName:  loginName,
		Role:       role,
		LastActive: m.now(),
	}
	m.sessions[token] = session
	return cloneSession(session), nil
}

func (m *Manager) Get(token string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneLocked()
	session, ok := m.sessions[token]
	if !ok {
		return nil, false
	}
	session.LastActive = m.now()
	return cloneSession(session), true
}

func (m *Manager) Delete(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	return len(m.sessions)
}

func (m *Manager) pruneLocked() {
	now := m.now()
	for token, session := range m.sessions {
		if now.Sub(session.LastActive) > m.idleTimeout {
			delete(m.sessions, token)
		}
	}
}

func cloneSession(session *Session) *Session {
	if session == nil {
		return nil
	}
	copy := *session
	return &copy
}

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
