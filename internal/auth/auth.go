package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

const (
	SessionCookieName = "xuiagg_session"
	SessionTTL        = 30 * 24 * time.Hour
	InviteTTL         = 7 * 24 * time.Hour
)

var (
	ErrBadCredentials = errors.New("invalid login or password")
	ErrInviteInvalid  = errors.New("invite invalid or expired")
)

type ctxKey int

const userKey ctxKey = 1

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

type Service struct {
	Store  *storage.Store
	Secure bool // true → cookies с Secure флагом (прод за HTTPS)
}

// Authenticate проверяет логин+пароль и возвращает пользователя.
func (s *Service) Authenticate(login, password string) (*storage.User, error) {
	u, err := s.Store.UserByLogin(login)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrBadCredentials
		}
		return nil, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return nil, ErrBadCredentials
	}
	return u, nil
}

// StartSession кладёт cookie сессии в ответ.
func (s *Service) StartSession(w http.ResponseWriter, userID int64) error {
	token, err := s.Store.CreateSession(userID, SessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(SessionTTL),
	})
	return nil
}

// EndSession удаляет сессию и cookie.
func (s *Service) EndSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		_ = s.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// User из запроса (если есть активная сессия).
func (s *Service) User(r *http.Request) *storage.User {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil
	}
	u, err := s.Store.SessionUser(c.Value)
	if err != nil {
		return nil
	}
	return u
}

// Middleware подкладывает User в context (если есть).
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := s.User(r); u != nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser защищает хендлер — редирект на /login если нет сессии.
func (s *Service) RequireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// RequireAdmin защищает хендлер — 403 если не админ.
func (s *Service) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !u.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// FromContext извлекает пользователя из контекста (Middleware должен быть применён).
func FromContext(ctx context.Context) *storage.User {
	u, _ := ctx.Value(userKey).(*storage.User)
	return u
}

// ValidateInvite проверяет что токен существует, не использован и не истёк.
func (s *Service) ValidateInvite(token string) (*storage.Invite, error) {
	inv, err := s.Store.InviteByToken(token)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrInviteInvalid
		}
		return nil, err
	}
	if inv.UsedAt != nil {
		return nil, ErrInviteInvalid
	}
	if time.Now().After(inv.ExpiresAt) {
		return nil, ErrInviteInvalid
	}
	return inv, nil
}
