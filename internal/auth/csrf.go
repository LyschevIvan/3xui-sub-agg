package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

const CSRFCookieName = "xuiagg_csrf"

// IssueCSRF выписывает новую csrf-cookie в ответ. Безопасное значение хранится
// в HttpOnly cookie и одновременно встраивается сервером в HTML-формы — на
// POST'е сравниваем cookie ↔ form (double-submit pattern). XSS приобретает
// доступ к API и без этого, поэтому HttpOnly здесь не повредит.
func (s *Service) IssueCSRF(w http.ResponseWriter) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Невероятный кейс — fallback на пустую строку,
		// в этом случае проверка не пройдёт и форму придётся обновить.
		return ""
	}
	tok := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(SessionTTL),
	})
	return tok
}

// CSRFToken возвращает текущее значение csrf-cookie, либо "" если её нет.
func CSRFToken(r *http.Request) string {
	c, err := r.Cookie(CSRFCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// EnsureCSRF гарантирует наличие csrf-cookie у аутентифицированного пользователя:
// если cookie отсутствует — выписывает новую и возвращает её значение, иначе
// возвращает существующее. Используется в render-цепочке, чтобы новые формы
// получили токен без дополнительного раунда.
func (s *Service) EnsureCSRF(w http.ResponseWriter, r *http.Request) string {
	if t := CSRFToken(r); t != "" {
		return t
	}
	return s.IssueCSRF(w)
}

// ValidateCSRF возвращает true, если форма принесла токен, совпадающий с cookie.
// Сравнение constant-time. На GET всегда true (защищаются только мутации).
func (s *Service) ValidateCSRF(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	cookieTok := CSRFToken(r)
	formTok := r.FormValue("csrf_token")
	if cookieTok == "" || formTok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookieTok), []byte(formTok)) == 1
}

// RequireCSRFOnly — обёртка для эндпоинтов, которым не нужна сессия,
// но нужна защита от CSRF (например, /logout: отсутствие сессии — нормально,
// мы просто чистим cookie, но запросто без токена).
func (s *Service) RequireCSRFOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ValidateCSRF(r) {
			http.Error(w, "csrf token invalid — обновите страницу", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
