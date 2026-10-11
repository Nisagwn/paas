package web

// Turkish strings of the project and deployment screens (Genel, Deploy'lar,
// deploy detayı, Ayarlar). Kept apart from tr.go so the labels of these
// screens can change without touching the shared message tables.

func init() {
	for k, v := range uxFlashes {
		flashes[k] = v
	}
}

// uxFlashes are the ?ok= messages added by these screens.
var uxFlashes = map[string]string{
	"env-bulk":       "Değişkenler kaydedildi. Yeni değerler bir sonraki deploy'da geçerli olur.",
	"redeploy-new":   "Yeni deploy kuyruğa alındı; aşağıda canlı olarak izleyebilirsin.",
	"promoted-new":   "Production'a taşınıyor: aynı imajla yeni bir deploy kuyruğa alındı ve aşağıda izleniyor.",
	"rollback-here":  "Canlı site artık bu deploy'u gösteriyor.",
	"promoted-here":  "Canlı site artık bu deploy'u gösteriyor.",
	"canceled-here":  "Deploy iptal edildi.",
	"cancel-pending": "İptal istendi; deploy birkaç saniye içinde durur.",
}

// Timeline steps of a deployment.
const (
	stepQueued = "queued"
	stepBuild  = "build"
	stepDeploy = "deploy"
	stepResult = "result"
)

var stepLabels = map[string]string{
	stepQueued: "Sıraya alındı",
	stepBuild:  "Build",
	stepDeploy: "Deploy",
}

// Step states (CSS classes of the timeline).
const (
	stDone    = "done"
	stCurrent = "current"
	stFailed  = "failed"
	stPending = "pending"
	stSkipped = "skipped"
)

var stepStateLabels = map[string]string{
	stDone:    "tamamlandı",
	stCurrent: "sürüyor",
	stFailed:  "başarısız",
	stPending: "bekliyor",
	stSkipped: "atlandı",
}

// Deploy list filters.
var deployStatusFilters = []filterOption{
	{"", "Tüm durumlar"},
	{"ready", "Hazır"},
	{"active", "Sürüyor"},
	{"failed", "Başarısız"},
	{"canceled", "İptal edildi"},
	{"retired", "Emekli"},
}

var deployEnvFilters = []filterOption{
	{"", "Tüm ortamlar"},
	{"production", "Production"},
	{"preview", "Önizleme"},
}

// Reasons shown on disabled actions.
const (
	reasonRole        = "Bu işlem için ekipte üye rolü gerekir."
	reasonNotReady    = "Deploy hazır olduğunda kullanılabilir."
	reasonFailed      = "Başarısız deploy'un çalışan bir kopyası yok; düzeltip yeniden deploy et."
	reasonCanceled    = "İptal edilen deploy'un çalışan bir kopyası yok."
	reasonRunning     = "Deploy sürerken kullanılamaz; bitmesini bekle ya da iptal et."
	reasonIsLive      = "Bu deploy zaten canlı."
	reasonRetired     = "Bu deploy emekliye ayrıldı ve artık çalışmıyor; önce yeniden deploy et."
	reasonNoPrevious  = "Geri dönülecek, hâlâ çalışan önceki bir production deploy'u yok."
	reasonNoProd      = "Henüz canlı bir deploy yok."
	reasonPreviewOnly = "Önizleme deploy'u canlıya geri alınamaz; \"Production'a taşı\"yı kullan."
	reasonTermRetired = "Emekliye ayrılan deploy'un çalışan kopyası yok."
	reasonTermBusy    = "Terminal, hazır bir deploy'un çalışan kopyasında açılır."
)

// Failure explanations: what went wrong, the likely cause and the next step.
var failureTexts = map[string][2]string{
	"build": {"Build komutu başarısız oldu",
		"Uygulamanın kurulumu ya da build komutu hata ile bitti. Genellikle derleme hatası, eksik bağımlılık ya da yanlış build komutu yüzündendir."},
	"fetch": {"Kaynak kod alınamadı",
		"Repo ya da commit çekilemedi. Repo silinmiş, adı değişmiş ya da erişim izni kaldırılmış olabilir; commit force-push ile silinmiş de olabilir."},
	"root": {"Kök dizin bulunamadı",
		"Build ayarlarındaki kök dizin bu commit'te yok ya da repo dışını gösteriyor."},
	"settings": {"Build ayarları geçersiz",
		"Kaydedilmiş build ayarlarından biri bu deploy'da kullanılamadı."},
	"start": {"Uygulama başlatılamadı",
		"İmaj build edildi ama uygulama hazır hâle gelmedi: start komutu hatalı olabilir, uygulama PORT ortam değişkenindeki portu dinlemiyor olabilir ya da açılışta çöküyor olabilir."},
	"timeout": {"Süre aşıldı",
		"Build ya da yayın izin verilen sürede bitmedi. Büyük bağımlılıklar, sonsuz döngüye giren bir build betiği ya da yavaş açılan bir uygulama buna yol açabilir."},
	"database": {"Veritabanı hazırlanamadı",
		"Deploy'un kullandığı veritabanı ya da önizleme kopyası hazırlanamadı."},
	"processes": {"Süreç tanımı kullanılamadı",
		"paas.yaml ya da Procfile içindeki süreç tanımları planın sınırlarını aşıyor ya da geçersiz."},
	"quota": {"Kaynak sınırına takıldı",
		"Küme yeni pod'ları oluşturamadı; projenin kaynak kotası dolmuş olabilir."},
	"platform": {"Platform hatası",
		"Deploy, uygulamandan bağımsız bir platform sorunu yüzünden tamamlanamadı."},
	"other": {"Deploy başarısız oldu",
		"Ayrıntılar log'un sonundaki hata satırlarında."},
	"canceled": {"Deploy iptal edildi",
		"Bu deploy tamamlanmadan durduruldu; canlı site etkilenmedi."},
}

var failureNext = map[string]string{
	"build":     "Log'daki ilk hatayı düzeltip push et. Build ya da install komutu yanlışsa Build ayarlarından değiştir.",
	"fetch":     "Reponun var olduğunu ve GitHub bağlantısının bu repoya erişebildiğini kontrol et, sonra yeniden deploy et.",
	"root":      "Build ayarlarında kök dizini düzelt ya da boş bırak, sonra yeniden deploy et.",
	"settings":  "Build ayarlarını kontrol edip kaydet, sonra yeniden deploy et.",
	"start":     "Start komutunu kontrol et; uygulamanın PORT ortam değişkenindeki portu dinlediğinden emin ol. Çalışma log'u açılıştaki hatayı gösterir.",
	"timeout":   "Build süresini kısalt ya da daha yüksek limitli bir plana geç; sonra yeniden deploy et.",
	"database":  "Veritabanları sekmesinde durumu kontrol et; hazır olduğunda yeniden deploy et.",
	"processes": "Süreç tanımlarını düzeltip push et.",
	"quota":     "Kullanılmayan önizlemeleri ya da süreç kopyalarını azalt; sonra yeniden deploy et.",
	"platform":  "Birkaç dakika sonra yeniden deploy et. Sorun sürerse platform yöneticisine haber ver.",
	"other":     "Hatayı düzeltip push et ya da yeniden deploy et.",
	"canceled":  "Bu commit'i yine yayınlamak istersen yeniden deploy et.",
}

// Env bulk paste messages.
const (
	envBulkEmpty   = "Yapıştırılan metinde değişken bulunamadı. Her satır AD=değer biçiminde olmalı."
	envBulkTooMany = "Tek seferde en fazla 200 değişken eklenebilir."
	envBulkLine    = "%d. satır: %s"
	envBulkSyntax  = "AD=değer biçiminde değil"
	envSaved       = "Kaydedildi. Yeni değerler bir sonraki deploy'da geçerli olur."
	envBulkSaved   = "%d değişken kaydedildi. Yeni değerler bir sonraki deploy'da geçerli olur."
	envDeleted     = "Değişken silindi. Değişiklik bir sonraki deploy'da geçerli olur."
	domainsSaved   = "Alan adları güncellendi."
)
