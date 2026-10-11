package web

import "net/http"

// Turkish texts of navigation and first use (nav.go, dashboard.go,
// import_ux.go, the error page). They live here so the parallel UI work
// on tr.go does not collide.

// Onboarding checklist of the dashboard.
const (
	obConnectTitle  = "GitHub'ı bağla"
	obConnectText   = "paas'ın repolarını okuyabilmesi için GitHub App'i kur ve hangi repolara erişebileceğini seç."
	obConnectAction = "GitHub'ı bağla"
	obConnectSkip   = "Bu sunucuda GitHub bağlantısı kapalı; repoyu bir sonraki adımda elle ekleyebilirsin."
	obPickTitle     = "Repo seç"
	obPickText      = "Deploy edilecek repoyu seç. Framework, build ve install komutları otomatik algılanır."
	obPickAction    = "Repo seç"
	obDeployTitle   = "İlk deploy"
	obDeployText    = "İlk deploy bitince projen canlı adresinde yayında olur."
	obDeployWatch   = "Deploy'u izle"
	obDeployFailed  = "Son deploy başarısız oldu. Log'a bakıp hatayı düzelt ve tekrar push et."
	obDeployLogs    = "Log'u aç"
	obDeployOpen    = "Projeyi aç"
)

// obDeployPush is the deploy step of a project that has no deployment yet.
func obDeployPush(branch string) string {
	return "Deploy'u başlatmak için " + branch + " branch'ine push et; paas her push'ta otomatik deploy eder."
}

// Import page.
const importRepoMissing = "Bu repo içe aktarılabilecek repolar arasında yok. GitHub'da repoya erişim verdiğinden ve ekipte üye rolün olduğundan emin ol."

func importRepoExisting(app string) string {
	return "Bu repo zaten " + app + " projesi olarak ekli."
}

// What to do about an error (import page, manual add).
var (
	helpLimit = errorHelp{Text: "Kullanım sayfasında planının limitlerini ve bu ayki kullanımını görebilirsin. " +
		"Limiti artırmak için platform yöneticisine yaz ya da kullanmadığın bir projeyi sil.",
		Href: "/usage", Label: "Kullanımı gör"}
	helpRole = errorHelp{Text: "Ekip sahibinden rolünü üye olarak değiştirmesini iste ya da kendi ekibini oluştur.",
		Href: "/teams", Label: "Ekiplere git"}
	helpName     = errorHelp{Text: "Proje adı benzersiz olmalı; farklı bir ad dene."}
	helpExisting = errorHelp{Text: "Bu repo için zaten bir proje var; yeni bir deploy için repoya push et.",
		Label: "Projeyi aç"}
	helpInstall = errorHelp{Text: "Repo listesi GitHub App'in erişebildiği repolardan gelir. GitHub'da repoya erişim ver; liste birkaç saniye içinde güncellenir.",
		Href: "/import", Label: "Repo listesine dön"}
)

// errorHints explain an error page and say what to do next.
var errorHints = map[int]string{
	http.StatusBadRequest:          "İstek eksik ya da hatalı. Bağlantıyı yeniden açmayı dene.",
	http.StatusUnauthorized:        "Bu sayfayı görmek için giriş yapmalısın.",
	http.StatusForbidden:           "Bu işlem için yetkin yok. Ekip sahibinden rolünü yükseltmesini isteyebilirsin.",
	http.StatusNotFound:            "Aradığın sayfa taşınmış, silinmiş ya da hiç var olmamış olabilir. Projeler başka bir ekibe aitse onları göremezsin.",
	http.StatusConflict:            "Aynı ada sahip bir kayıt zaten var.",
	http.StatusTooManyRequests:     "Kısa sürede çok fazla istek geldi. Biraz bekleyip tekrar dene.",
	http.StatusInternalServerError: "Sorun bizde. Birkaç saniye sonra tekrar dene; sürerse platform yöneticisine haber ver.",
	http.StatusNotImplemented:      "Bu özellik bu sunucuda açık değil.",
	http.StatusBadGateway:          "GitHub ya da başka bir servisle bağlantı kurulamadı. Biraz sonra tekrar dene.",
}

// errorData is the error page's data: the status code and a hint.
func errorData(status int) map[string]any {
	return map[string]any{"Status": status, "Hint": errorHints[status]}
}

// Team page sections (teamtabs).
const (
	teamTabsLabel        = "Ekip bölümleri"
	teamTabGeneral       = "Genel"
	teamTabNotifications = "Bildirimler"
	teamTabUsage         = "Plan ve kullanım"
)
