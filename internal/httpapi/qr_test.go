package httpapi

import (
	"bytes"
	"context"
	"image/png"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	gozxingqr "github.com/makiuchi-d/gozxing/qrcode"

	"github.com/Skufik2007/url-shortener/internal/model"
)

func TestQRCode(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com/x", "alias": "promo"})

	resp, body := e.do(t, "GET", "/qr/promo.png?size=300", "", nil)
	wantStatus(t, resp, body, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Картинка — валидный PNG, а QR-код расшифровывается ровно в короткую ссылку.
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("некорректный PNG: %v", err)
	}
	if w := img.Bounds().Dx(); w > 300 || w < 200 {
		t.Errorf("ширина %d, ожидалось не больше 300", w)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := gozxingqr.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		t.Fatalf("QR-код не распознан: %v", err)
	}
	if got := res.GetText(); got != "http://sho.rt/promo" {
		t.Errorf("в QR-коде %q, ожидалось %q", got, "http://sho.rt/promo")
	}

	// Выдача QR-кода не считается переходом.
	if len(e.tracker.events) != 0 {
		t.Errorf("QR-код попал в аналитику: %+v", e.tracker.events)
	}
	if e.metrics.QRGenerated.Load() != 1 || e.metrics.Redirects.Load() != 0 {
		t.Errorf("счётчики: qr=%d redirects=%d", e.metrics.QRGenerated.Load(), e.metrics.Redirects.Load())
	}
}

func TestQRCodeErrors(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "del-me"})
	e.do(t, "DELETE", "/api/v1/links/del-me", tok, nil)
	past := time.Now().Add(-time.Hour)
	_ = e.store.Create(context.Background(), &model.Link{Code: "old", OriginalURL: "https://example.com", OwnerID: 1, IsActive: true, ExpiresAt: &past})
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "ok-link"})

	tests := []struct {
		name, path string
		want       int
	}{
		{"несуществующая ссылка", "/qr/nope123.png", 404},
		{"удалённая ссылка", "/qr/del-me.png", 410},
		{"истёкшая ссылка", "/qr/old.png", 410},
		{"нет расширения .png", "/qr/ok-link", 404},
		{"недопустимый код", "/qr/" + string(bytes.Repeat([]byte("a"), 40)) + ".png", 404},
		{"размер не число", "/qr/ok-link.png?size=big", 400},
		{"размер меньше минимума", "/qr/ok-link.png?size=10", 400},
		{"размер больше максимума", "/qr/ok-link.png?size=5000", 400},
		{"размер на границе", "/qr/ok-link.png?size=64", 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do(t, "GET", tc.path, "", nil)
			wantStatus(t, resp, body, tc.want)
		})
	}
}
