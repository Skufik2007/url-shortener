package analytics

import "strings"

// ParseUA определяет браузер, ОС и тип устройства по строке User-Agent.
// Используются простые эвристики без внешних библиотек; порядок проверок важен:
// Edge/Opera/Yandex/Samsung содержат токен Chrome, а Chrome — токен Safari.
func ParseUA(ua string) (browser, os, device string) {
	return parseBrowser(ua), parseOS(ua), parseDevice(ua)
}

func parseBrowser(ua string) string {
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"), strings.Contains(ua, "EdgiOS/"), strings.Contains(ua, "Edge/"):
		return "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"), strings.Contains(ua, "OPT/"):
		return "Opera"
	case strings.Contains(ua, "YaBrowser"):
		return "Yandex"
	case strings.Contains(ua, "SamsungBrowser"):
		return "Samsung Internet"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		return "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"), strings.Contains(ua, "Chromium/"):
		return "Chrome"
	case strings.Contains(ua, "Safari/"):
		return "Safari"
	}
	return "Other"
}

func parseOS(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		return "iOS"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "CrOS"):
		return "ChromeOS"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		return "macOS"
	case strings.Contains(ua, "Linux"), strings.Contains(ua, "X11"):
		return "Linux"
	}
	return "Other"
}

func parseDevice(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "bot"
	}
	l := strings.ToLower(ua)
	for _, k := range []string{"bot", "crawler", "spider", "curl", "wget", "python-requests", "go-http-client"} {
		if strings.Contains(l, k) {
			return "bot"
		}
	}
	switch {
	case strings.Contains(ua, "iPad"):
		return "tablet"
	case strings.Contains(ua, "Android") && !strings.Contains(ua, "Mobile"):
		return "tablet"
	case strings.Contains(ua, "Mobile"), strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPod"):
		return "mobile"
	}
	return "desktop"
}
