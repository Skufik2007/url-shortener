package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Skufik2007/url-shortener/internal/auth"
	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/qrcode"
	"github.com/Skufik2007/url-shortener/internal/service"
)

// ---- вспомогательные функции ответа ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON читает тело запроса; при ошибке сам пишет ответ и возвращает false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}

// ---- middleware уровня маршрута ----

type userHandler func(w http.ResponseWriter, r *http.Request, userID int64)

// authed требует заголовок Authorization: Bearer <токен>.
func (s *server) authed(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shortener"`)
			writeError(w, http.StatusUnauthorized, "authorization required")
			return
		}
		id, err := s.Auth.ParseToken(strings.TrimSpace(h[len(prefix):]))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shortener", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r, id)
	}
}

// limited применяет ограничитель частоты по IP клиента.
func (s *server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, wait := s.limiter.Allow(clientIP(r, s.TrustProxy))
		if !ok {
			s.Metrics.RateLimited.Add(1)
			secs := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next(w, r)
	}
}

// ---- сопоставление ошибок ----

func (s *server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidURL),
		errors.Is(err, service.ErrBadAlias),
		errors.Is(err, service.ErrBadPeriod):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "link not found")
	case errors.Is(err, service.ErrGone):
		writeError(w, http.StatusGone, "link expired or disabled")
	case errors.Is(err, service.ErrAliasTaken):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("внутренняя ошибка",
			"request_id", requestIDFrom(r.Context()), "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid email or password")
	case errors.Is(err, auth.ErrEmailTaken):
		writeError(w, http.StatusConflict, "email already registered")
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "password must be 8 to 72 bytes long")
	case errors.Is(err, auth.ErrInvalidEmail):
		writeError(w, http.StatusBadRequest, "invalid email")
	default:
		s.log.Error("ошибка аутентификации",
			"request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// ---- аутентификация ----

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	Token string      `json:"token"`
	User  *model.User `json:"user"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	u, token, err := s.Auth.Register(r.Context(), c.Email, c.Password)
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, authResponse{Token: token, User: u})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	u, token, err := s.Auth.Login(r.Context(), c.Email, c.Password)
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, authResponse{Token: token, User: u})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request, userID int64) {
	// Профиль в Authenticator не запрашиваем: достаточно идентификатора из токена.
	writeJSON(w, http.StatusOK, map[string]int64{"id": userID})
}

// ---- ссылки ----

// linkResponse — ссылка вместе с готовым коротким адресом.
type linkResponse struct {
	model.Link
	ShortURL string `json:"short_url"`
}

func (s *server) toResponse(l model.Link) linkResponse {
	return linkResponse{Link: l, ShortURL: s.BaseURL + "/" + l.Code}
}

func (s *server) handleCreateLink(w http.ResponseWriter, r *http.Request, userID int64) {
	var in service.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	l, err := s.Links.Create(r.Context(), userID, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.Metrics.LinksCreated.Add(1)
	w.Header().Set("Location", s.BaseURL+"/api/v1/links/"+l.Code)
	writeJSON(w, http.StatusCreated, s.toResponse(*l))
}

func (s *server) handleListLinks(w http.ResponseWriter, r *http.Request, userID int64) {
	limit, offset := 0, 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	links, err := s.Links.List(r.Context(), userID, limit, offset)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]linkResponse, 0, len(links))
	for _, l := range links {
		out = append(out, s.toResponse(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (s *server) handleGetLink(w http.ResponseWriter, r *http.Request, userID int64) {
	l, err := s.Links.Get(r.Context(), userID, r.PathValue("code"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toResponse(*l))
}

func (s *server) handleDeleteLink(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := s.Links.Delete(r.Context(), userID, r.PathValue("code")); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const dateLayout = "2006-01-02"

func (s *server) handleStats(w http.ResponseWriter, r *http.Request, userID int64) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	toDay := today
	fromDay := today.AddDate(0, 0, -29)

	q := r.URL.Query()
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'to' date, expected YYYY-MM-DD")
			return
		}
		toDay = t
		if q.Get("from") == "" {
			fromDay = toDay.AddDate(0, 0, -29)
		}
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'from' date, expected YYYY-MM-DD")
			return
		}
		fromDay = t
	}
	// «to» включительно: сервис работает с полуинтервалом [from, to).
	st, err := s.Links.Stats(r.Context(), userID, r.PathValue("code"), fromDay, toDay.Add(24*time.Hour))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) handleTop(w http.ResponseWriter, r *http.Request) {
	n := 10
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "n must be between 1 and 100")
			return
		}
		n = parsed
	}
	codes, err := s.Links.TopLinks(r.Context(), n)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if codes == nil {
		codes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"codes": codes})
}

// ---- редирект ----

func (s *server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	v := service.Visit{
		IP:        clientIP(r, s.TrustProxy),
		UserAgent: r.UserAgent(),
		Referer:   r.Referer(),
	}
	if s.TrustProxy {
		// Заголовок CDN можно доверять только за собственным прокси.
		v.CountryHint = r.Header.Get("CF-IPCountry")
	}
	target, err := s.Links.Resolve(r.Context(), r.PathValue("code"), v)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNotFound):
			s.Metrics.RedirectMissing.Add(1)
		case errors.Is(err, service.ErrGone):
			s.Metrics.RedirectGone.Add(1)
		}
		s.writeServiceError(w, r, err)
		return
	}
	s.Metrics.Redirects.Add(1)
	// Браузер не должен кэшировать редирект, иначе повторные переходы не учтутся.
	w.Header().Set("Cache-Control", "private, max-age=0")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// ---- QR-код ----

// handleQR отдаёт PNG с QR-кодом короткой ссылки: GET /qr/{code}.png?size=256.
// Адрес публичный: в QR зашита только короткая ссылка, которая и так открыта.
func (s *server) handleQR(w http.ResponseWriter, r *http.Request) {
	code, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}
	size := qrcode.DefaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < qrcode.MinSize || parsed > qrcode.MaxSize {
			writeError(w, http.StatusBadRequest,
				"size must be between "+strconv.Itoa(qrcode.MinSize)+" and "+strconv.Itoa(qrcode.MaxSize))
			return
		}
		size = parsed
	}
	if err := s.Links.CheckActive(r.Context(), code); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	img, err := qrcode.PNG(s.BaseURL+"/"+code, size)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.Metrics.QRGenerated.Add(1)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	// Картинка зависит только от кода и размера, её можно кэшировать.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(img)
}

// ---- служебные ----

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status := http.StatusOK
	checks := make(map[string]string, len(s.Health))
	for _, c := range s.Health {
		if err := c.Ping(ctx); err != nil {
			checks[c.Name] = "error"
			status = http.StatusServiceUnavailable
			s.log.Warn("healthcheck не пройден", "check", c.Name, "err", err)
		} else {
			checks[c.Name] = "ok"
		}
	}
	state := "ok"
	if status != http.StatusOK {
		state = "unavailable"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": checks})
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.Metrics.WritePrometheus(w, s.CollectorStats)
}
