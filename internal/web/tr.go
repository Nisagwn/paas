package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The UI is in Turkish. Stored values (statuses, roles, alias kinds) keep
// their English identifiers and get Turkish labels here; messages that come
// from other packages (internal/api, internal/auth, internal/domains, which
// also answer the JSON API in English) or from github.go are translated by
// tr before they reach a page. Unknown text passes through unchanged.

var statusLabels = map[string]string{
	// Deployments.
	"queued":    "Sırada",
	"building":  "Kuruluyor",
	"deploying": "Yayına alınıyor",
	"ready":     "Hazır",
	"failed":    "Başarısız",
	"retired":   "Emekli",
	"canceled":  "İptal edildi",
	"sleeping":  "Uykuda",
	// Custom domains.
	"pending":  "Bekliyor",
	"verified": "Doğrulandı",
	"active":   "Aktif",
	"error":    "Hata",
}

var roleLabels = map[string]string{
	"owner":  "Sahip",
	"member": "Üye",
	"viewer": "İzleyici",
}

var kindLabels = map[string]string{
	"production": "Canlı",
	"preview":    "Önizleme",
	"custom":     "Alan adı",
}

func label(m map[string]string) func(string) string {
	return func(s string) string {
		if l, ok := m[s]; ok {
			return l
		}
		return s
	}
}

// Faz 17 environments and origins, Faz 19 health.
var targetLabels = map[string]string{
	"production": "Production",
	"preview":    "Önizleme",
}

var originLabels = map[string]string{
	"git":      "Push",
	"redeploy": "Yeniden deploy",
	"promote":  "Production'a taşındı",
	"hook":     "Deploy hook'u",
}

var healthLabels = map[string]string{
	"healthy":    "Sağlıklı",
	"degraded":   "Sorunlu",
	"failing":    "Çöküyor",
	"no_traffic": "Trafik yok",
}

// settingsFields names the build settings fields in error messages.
var settingsFields = map[string]string{
	"root_directory":   "Kök dizin",
	"output_directory": "Çıktı dizini",
	"install_command":  "Install komutu",
	"build_command":    "Build komutu",
	"start_command":    "Start komutu",
}

var (
	statusLabel = label(statusLabels)
	roleLabel   = label(roleLabels)
	kindLabel   = label(kindLabels)
	targetLabel = label(targetLabels)
	originLabel = label(originLabels)
	healthLabel = label(healthLabels)
	fieldLabel  = label(settingsFields)
)

// statusTitles are the headings of error pages.
var statusTitles = map[int]string{
	http.StatusBadRequest:          "Geçersiz istek",
	http.StatusUnauthorized:        "Giriş gerekli",
	http.StatusForbidden:           "Erişim yok",
	http.StatusNotFound:            "Bulunamadı",
	http.StatusConflict:            "Çakışma",
	http.StatusInternalServerError: "Bir şeyler ters gitti",
	http.StatusNotImplemented:      "Kullanılamıyor",
	http.StatusBadGateway:          "Bağlantı hatası",
}

func statusTitle(code int) string {
	if t, ok := statusTitles[code]; ok {
		return t
	}
	return http.StatusText(code)
}

// messages maps normalized English messages (see norm) to Turkish.
var messages = map[string]string{
	// internal/api: import.
	`repo must look like "owner/repo"`: `Repo "sahip/repo" biçiminde olmalı.`,
	"repository not found among the GitHub App installations of your teams; install the GitHub App on it first": "Bu repoya erişim yok. Önce GitHub'da bu repoyu seç.",
	"team not found": "Ekip bulunamadı.",
	"importing apps needs the member role on the team":                                "İçe aktarmak için ekipte üye rolü gerekir.",
	"name must be 2-31 chars: lowercase letters, digits, '-', starting with a letter": "Ad 2-31 karakter olmalı: a-z harfleri, rakamlar ve '-'; harfle başlamalı.",
	"invalid branch name":                          "Geçersiz branch adı.",
	"an app with this name or repo already exists": "Bu ad ya da repo ile bir proje zaten var.",
	// internal/auth: teams and tokens.
	"team slug must be 2-31 chars: lowercase letters, digits, '-', starting with a letter": "Kısa ad 2-31 karakter olmalı: a-z harfleri, rakamlar ve '-'; harfle başlamalı.",
	"team name is longer than 100 characters":                                              "Ekip adı en fazla 100 karakter olabilir.",
	"role must be owner, member or viewer":                                                 "Rol sahip, üye ya da izleyici olmalı.",
	"invalid GitHub login":                                                                 "Geçersiz GitHub kullanıcı adı.",
	"personal tokens belong to a user; sign in with GitHub":                                "Kişisel token için GitHub ile giriş yapmalısın.",
	"token name must be 1-100 characters":                                                  "Token adı 1-100 karakter olmalı.",
	"expires_in_days must be between 0 (never) and 3650":                                   "Süre 1 ile 3650 gün arasında olmalı (boş: süresiz).",
	// internal/domains: hostnames and DNS records.
	"hostname is required": "Alan adı gerekli.",
	"hostname only: no scheme, port or path (e.g. www.example.com)":         "Yalnızca alan adını yaz: http://, port ya da yol olmadan (ör. www.ornek.com).",
	"wildcard domains are not supported":                                    "Joker (*) alan adları desteklenmiyor.",
	"hostname is longer than 253 characters":                                "Alan adı en fazla 253 karakter olabilir.",
	"use the punycode (xn--) form of internationalized domain names":        "Türkçe karakterli alan adlarında punycode (xn--) biçimini kullan.",
	"hostname must be a fully qualified domain name (e.g. www.example.com)": "Tam bir alan adı yaz (ör. www.ornek.com).",
	"IP addresses are not domain names":                                     "IP adresi alan adı değildir.",
	"an app can have at most 20 custom domains":                             "Bir projeye en fazla 20 alan adı eklenebilir.",
	"recommended: routes traffic to the platform and verifies the domain":   "Önerilen: trafiği platforma yönlendirir ve alan adını doğrular.",
	"domain verification is not running":                                    "Alan adı doğrulama şu an çalışmıyor.",
	"waiting for the first production deployment":                           "İlk canlı deploy bekleniyor.",
	// github.go.
	"import from GitHub": "GitHub'dan içe aktar",
	"signing in":         "Giriş yapılıyor",
	"gitHub App":         "GitHub",
	"the GitHub App is not configured, or GitHub sign-in is off":            "GitHub bağlantısı ayarlı değil ya da GitHub ile giriş kapalı.",
	"installing the GitHub App needs a GitHub sign-in, not the admin token": "GitHub'ı bağlamak için admin token'ıyla değil, GitHub ile giriş yapmalısın.",
	"installing the GitHub App needs the member role on the team":           "GitHub'ı bağlamak için ekipte üye rolü gerekir.",
	"linking a GitHub App installation needs GitHub sign-in (PAAS_GITHUB_OAUTH_CLIENT_ID and PAAS_GITHUB_OAUTH_CLIENT_SECRET, the App's client credentials). In development mode, create apps with New app and a repository webhook instead": "GitHub'ı bağlamak için GitHub ile giriş açık olmalı (PAAS_GITHUB_OAUTH_CLIENT_ID ve PAAS_GITHUB_OAUTH_CLIENT_SECRET). Geliştirme modunda projeyi ana sayfadaki \"Gelişmiş: elle ekle\" bölümünden ekle.",
	"gitHub did not send an installation id": "GitHub kurulum numarası göndermedi.",
	"gitHub App settings saved":              "GitHub ayarları kaydedildi.",
	"this installation link expired or was not started here. Open Import and click Install GitHub App again: GitHub shows the existing installation and sends you back to link it to your team": "Bu bağlantının süresi doldu ya da burada başlatılmadı. \"Yeni proje\" sayfasında tekrar \"GitHub'da repo seç\"e bas; GitHub seni geri gönderir ve bağlantı ekibine eklenir.",
	"gitHub authorization was cancelled; the installation is not linked":       "GitHub izni iptal edildi; bağlantı kurulmadı.",
	"checking the installation with GitHub failed. The server log has details": "GitHub ile kontrol başarısız oldu. Ayrıntılar sunucu logunda.",
	"gitHub App installed":          "GitHub bağlandı.",
	"your account no longer exists": "Hesabın artık yok.",
	"linking a GitHub App installation needs the member role on the team": "GitHub'ı bağlamak için ekipte üye rolü gerekir.",
	"your GitHub account cannot access this installation":                 "GitHub hesabının bu bağlantıya erişimi yok.",
	"this installation is already linked to another team":                 "Bu GitHub bağlantısı başka bir ekibe ait.",
	// Faz 16–19 screens: build settings, environments, deploy controls, hooks, analytics.
	`node_version must look like "22", "20.18" or "lts"`:                           `Node sürümü "22", "20.18" ya da "lts" biçiminde olmalı.`,
	`target must be "production", "preview" or "all"`:                              "Ortam Tümü, Production ya da Önizleme olmalı.",
	`git_branch needs target "preview"`:                                            "Branch yalnızca Önizleme ortamı için seçilebilir.",
	"git_branch is too long":                                                       "Branch adı en fazla 255 karakter olabilir.",
	"name must be at most 100 characters and branch a valid branch name":           "Ad en fazla 100 karakter, branch geçerli bir branch adı olmalı.",
	"deployment is retired: redeploy it first":                                     "Bu deploy emekliye ayrıldı; önce yeniden deploy et.",
	"range must be 1h, 24h or 7d":                                                  "Aralık 1 sa, 24 sa ya da 7 gün olmalı.",
	"resource usage needs the kubernetes deployer":                                 "Kullanım verisi yalnızca Kubernetes ile çalışırken okunur.",
	"resource metrics unavailable: metrics-server (metrics.k8s.io) is not running": "Kullanım verisi yok: kümede metrics-server çalışmıyor.",
}

var normMessages = func() map[string]string {
	m := make(map[string]string, len(messages))
	for k, v := range messages {
		m[norm(k)] = v
	}
	return m
}()

// patterns translate messages with a variable part.
// Captured groups pass through label: field names of the build settings
// and deployment statuses are translated, anything else is kept.
var patterns = []struct {
	re *regexp.Regexp
	tr string
}{
	// Faz 16–19 screens: build.NormalizeSettings.
	{regexp.MustCompile(`^(\w+) is longer than (\d+) characters$`), "%s en fazla %s karakter olabilir."},
	{regexp.MustCompile(`^(\w+) must be relative \(no leading "/"\)$`), `%s göreli olmalı (başta "/" olmadan).`},
	{regexp.MustCompile(`^(\w+) must use "/" as the separator$`), `%s içinde ayraç olarak "/" kullan.`},
	{regexp.MustCompile(`^(\w+) must stay inside the repository \(no "\.\."\)$`), `%s repo içinde kalmalı (".." olmadan).`},
	{regexp.MustCompile(`^(\w+) may only contain letters, digits and \. _ @ \+ -$`), "%s yalnızca harf, rakam ve . _ @ + - içerebilir."},
	{regexp.MustCompile(`^(\w+) must be a single line of printable characters$`), "%s tek satır olmalı ve yalnızca yazdırılabilir karakter içermeli."},
	{regexp.MustCompile(`^(\w+) must not end with a backslash$`), `%s "\" ile bitemez.`},
	{regexp.MustCompile(`^framework must be empty \(auto-detect\) or one of: (.+)$`), "Framework boş (otomatik algıla) ya da şunlardan biri olmalı: %s."},
	// Faz 16–19 screens: deploy controls (api.Promote, Redeploy, Cancel).
	{regexp.MustCompile(`^only ready deployments can be promoted \(status (\w+)\)$`), "Yalnızca hazır deploy'lar production'a taşınabilir (durum: %s)."},
	{regexp.MustCompile(`^deployment is still (\w+); cancel it or wait until it finishes$`), "Deploy hâlâ sürüyor (%s); iptal et ya da bitmesini bekle."},
	{regexp.MustCompile(`^deployment already finished \((\w+)\)$`), "Deploy zaten bitti (%s)."},
	{regexp.MustCompile(`(?i)^invalid variable name (".*")$`), "Geçersiz değişken adı: %s."},
	{regexp.MustCompile(`(?i)^(.+) is set by the platform$`), "%s platform tarafından ayarlanır."},
	{regexp.MustCompile(`(?i)^(.+) is longer than 32 KiB$`), "%s en fazla 32 KiB olabilir."},
	{regexp.MustCompile(`(?i)^invalid label (".*"): use 1-63 letters, digits and '-', not at either end$`), "Geçersiz alan adı parçası %s: 1-63 harf, rakam ve '-' kullan; '-' başta ya da sonda olamaz."},
	{regexp.MustCompile(`(?i)^hostnames under (.+) belong to the platform$`), "%s altındaki adlar platforma ait."},
	{regexp.MustCompile(`(?i)^(.+) is already added to an app$`), "%s zaten bir projeye eklenmiş."},
	{regexp.MustCompile(`(?i)^waiting for the certificate: (.+)$`), "Sertifika bekleniyor: %s."},
	{regexp.MustCompile(`(?i)^this repository is already deployed as app (.+)$`), "Bu repo zaten %s projesi olarak ekli."},
	{regexp.MustCompile(`(?i)^alternative for apex domains: verifies ownership; then point an A \(or ALIAS\) record at the address of (.+)$`), "Kök alan adı için: sahipliği doğrular; sonra bir A (ya da ALIAS) kaydını %s adresine yönlendir."},
	{regexp.MustCompile(`(?i)^gitHub authorized a different account than the one signed in here\. Sign in to GitHub as (@\S+) and try again$`), "GitHub burada giriş yapandan farklı bir hesabı onayladı. GitHub'a %s olarak giriş yapıp tekrar dene."},
}

// norm lowercases the first letter and drops a final period, so "Team not
// found." and "team not found" are the same key.
func norm(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// tr translates a known English message; anything else is returned as is.
func tr(s string) string {
	if s == "" {
		return s
	}
	k := norm(s)
	if t, ok := normMessages[k]; ok {
		return t
	}
	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(strings.TrimSuffix(strings.TrimSpace(s), ".")); m != nil {
			args := make([]any, len(m)-1)
			for i, g := range m[1:] {
				if l := fieldLabel(g); l != g {
					args[i] = l
				} else {
					args[i] = statusLabel(g)
				}
			}
			return fmt.Sprintf(p.tr, args...)
		}
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "az önce"
	case d < time.Hour:
		return fmt.Sprintf("%d dk önce", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d sa önce", int(d.Hours()))
	default:
		return fmt.Sprintf("%d gün önce", int(d.Hours()/24))
	}
}

// shortDuration formats a build time: "12 sn", "3 dk 5 sn", "1 sa 2 dk".
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%d sa %d dk", h, m)
	case m > 0 && s > 0:
		return fmt.Sprintf("%d dk %d sn", m, s)
	case m > 0:
		return fmt.Sprintf("%d dk", m)
	default:
		return fmt.Sprintf("%d sn", s)
	}
}
