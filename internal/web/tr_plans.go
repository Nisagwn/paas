package web

import (
	"net/http"
	"regexp"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 23: Turkish texts of the plan limits, rate limits and suspension.
// They extend the tables of tr.go here, so the phases editing tr.go in
// parallel do not collide.

var planMessages = map[string]string{
	"your account is suspended":                                           "Hesabın askıya alındı.",
	"too many requests; try again later":                                  "Çok fazla istek. Biraz bekleyip tekrar dene.",
	"too many new projects; try again later":                              "Kısa sürede çok fazla proje oluşturdun. Biraz bekleyip tekrar dene.",
	`plan must be "hobby", "pro" or "unlimited"`:                          "Plan Hobby, Pro ya da Sınırsız olmalı.",
	"platform admins cannot be suspended":                                 "Platform yöneticileri askıya alınamaz.",
	"reason must be at most 500 characters":                               "Gerekçe en fazla 500 karakter olabilir.",
	"limits must be key=value lines (e.g. projects=50)":                   "Limitler anahtar=değer satırları olmalı (ör. projects=50).",
	"the team is suspended: deployments and new resources are disabled":   "Ekip askıya alındı: deploy ve yeni kaynak oluşturma kapalı.",
	`limit cpu must be a Kubernetes quantity such as "500m" or "2"`:       `CPU limiti "500m" ya da "2" gibi bir Kubernetes miktarı olmalı.`,
	`limit memory must be a Kubernetes quantity such as "512Mi" or "2Gi"`: `Bellek limiti "512Mi" ya da "2Gi" gibi bir Kubernetes miktarı olmalı.`,
}

var planPatterns = []struct {
	re *regexp.Regexp
	tr string
}{
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) projects per team$`),
		"Plan limitine ulaşıldı: %s planında ekip başına en fazla %s proje olabilir. Kullanım sayfasından durumu görebilirsin."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) deployments per day$`),
		"Plan limitine ulaşıldı: %s planında günde en fazla %s deploy yapılabilir. Sayaç 00:00 UTC'de sıfırlanır."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) build minutes per month$`),
		"Plan limitine ulaşıldı: %s planının aylık %s build dakikası bu ay doldu. Sayaç ayın ilk günü (UTC) sıfırlanır."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) custom domains per team$`),
		"Plan limitine ulaşıldı: %s planında ekip başına en fazla %s alan adı eklenebilir."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) databases per team$`),
		"Plan limitine ulaşıldı: %s planında ekip başına en fazla %s veritabanı oluşturulabilir."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) replicas per process; paas\.yaml asks for (\d+) replicas of (\S+)$`),
		"Plan limitine ulaşıldı: %s planında süreç başına en fazla %s kopya çalışabilir; paas.yaml %s kopya istiyor (%s)."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) replicas per process$`),
		"Plan limitine ulaşıldı: %s planında süreç başına en fazla %s kopya çalışabilir."},
	{regexp.MustCompile(`^plan limit reached: the (\w+) plan allows (\d+) cron jobs; paas\.yaml declares (\d+)$`),
		"Plan limitine ulaşıldı: %s planında en fazla %s cron işi olabilir; paas.yaml %s tane tanımlıyor."},
	{regexp.MustCompile(`^the team is suspended: deployments and new resources are disabled \((.*)\)$`),
		"Ekip askıya alındı: deploy ve yeni kaynak oluşturma kapalı (%s)."},
	{regexp.MustCompile(`^(?:build: )?the build exceeded the plan's build timeout of (.+)$`),
		"Build, planın %s olan build süre sınırını aştı."},
	{regexp.MustCompile(`^limit (\w+) must be a non-negative integer or "unlimited"$`),
		`%s limiti negatif olmayan bir tam sayı ya da "unlimited" olmalı.`},
	{regexp.MustCompile(`^unknown limit "(.+)"$`), "Bilinmeyen limit: %s."},
}

func init() {
	for k, v := range planMessages {
		messages[k] = v
		normMessages[norm(k)] = v
	}
	patterns = append(patterns, planPatterns...)
	statusTitles[http.StatusTooManyRequests] = "Çok fazla istek"
	loginErrors["suspended"] = "Hesabın bir yönetici tarafından askıya alındı. Sorun olduğunu düşünüyorsan platform yöneticisine yaz."
	loginErrors["too_new"] = "Bu GitHub hesabı yeni kayıt için çok yeni. Bir süre sonra tekrar dene ya da bir ekip sahibinden seni eklemesini iste."
	loginErrors["too_many"] = "Çok fazla giriş denemesi. Biraz bekleyip tekrar dene."
	flashes["imported-limit"] = "Proje oluşturuldu, ama plan limiti dolduğu için ilk deploy başlatılmadı. Kullanım sayfasından durumu görebilirsin."
	flashes["admin"] = "Kaydedildi."
	planLabels[store.PlanUnlimited] = "Sınırsız"
}

// planRefused renders a plan refusal on an app tab; false when err is nil.
func (s *Server) planRefused(w http.ResponseWriter, r *http.Request, app store.App, tab string, err error) bool {
	if err == nil {
		return false
	}
	if status, msg, ok := limitError(err); ok {
		s.renderTabOf(w, r, app, status, tab, msg, nil)
		return true
	}
	s.internalError(w, r, err)
	return true
}
