package web

import (
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// Turkish texts of the team, account and administration screens (teams,
// team, tokens, usage, admin, notifications, protection login, legal).

func init() {
	for k, v := range map[string]string{
		"team":           "Ekip oluşturuldu. Şimdi üye ekleyebilir ya da GitHub'dan bir proje bağlayabilirsin.",
		"member":         "Ekip üyeleri güncellendi.",
		"member-added":   "Üye eklendi. GitHub ile giriş yaptığında ekibi ve projelerini görür.",
		"member-role":    "Rol değiştirildi. Yeni yetkiler hemen geçerli.",
		"member-removed": "Üye ekipten çıkarıldı; ekibin projelerine artık erişemez.",
		"left-team":      "Ekipten ayrıldın.",
		"revoked":        "Token iptal edildi. Bu token'ı kullanan betikler artık giriş yapamaz.",
	} {
		flashes[k] = v
	}
	funcs["roledesc"] = func(r string) string { return roleDescriptions[r] }
	funcs["planinfo"] = func(p string) string { return planDescriptions[p] }
}

// roleDescriptions say in one line what each team role may do.
var roleDescriptions = map[string]string{
	store.RoleViewer: "Projeleri, deploy'ları ve logları görür; önizlemelere yorum yazar.",
	store.RoleMember: "İzleyicinin yaptıklarına ek olarak deploy eder, proje ve ayarlarını yönetir.",
	store.RoleOwner:  "Üyenin yaptıklarına ek olarak üye ekler, rolleri değiştirir ve üye çıkarır.",
}

// roleCapability is one row of the team page's role table.
type roleCapability struct {
	Label                 string
	Viewer, Member, Owner bool
}

var roleCapabilities = []roleCapability{
	{"Projeleri, deploy'ları, logları ve analitiği görmek", true, true, true},
	{"Önizlemelere yorum yazmak", true, true, true},
	{"Kendi e-posta bildirim tercihlerini seçmek", true, true, true},
	{"Proje oluşturmak, deploy etmek, geri almak", false, true, true},
	{"Ortam değişkenleri, alan adları, veritabanları, build ayarları", false, true, true},
	{"Bildirim kanalları ve GitHub bağlantısı", false, true, true},
	{"Üye eklemek, rol değiştirmek, üye çıkarmak", false, false, true},
}

// planDescriptions explain the plans on the usage page.
var planDescriptions = map[string]string{
	store.PlanHobby:     "Kişisel projeler ve denemeler için başlangıç planı.",
	store.PlanPro:       "Ekipler ve production iş yükleri için yüksek limitler.",
	store.PlanUnlimited: "Limit uygulanmaz; platform yöneticileri tarafından atanır.",
}

// channelKinds are the "Kanal ekle" choices with where to get each URL.
type channelKind struct {
	Kind, Label, Icon, Lead, Help, Placeholder string
	URL                                        bool
}

var channelKinds = []channelKind{
	{Kind: store.ChannelSlack, Label: "Slack", Icon: "#", URL: true,
		Lead:        "Bir Slack kanalına mesaj.",
		Help:        "Slack'te Uygulamalar → Incoming Webhooks → Slack'e ekle adımlarıyla bir kanal seç ve Webhook URL'sini kopyala.",
		Placeholder: "https://hooks.slack.com/services/…"},
	{Kind: store.ChannelDiscord, Label: "Discord", Icon: "◆", URL: true,
		Lead:        "Bir Discord kanalına mesaj.",
		Help:        "Discord'da kanal ayarları → Entegrasyonlar → Webhook'lar → Yeni Webhook → Webhook URL'sini Kopyala.",
		Placeholder: "https://discord.com/api/webhooks/…"},
	{Kind: store.ChannelWebhook, Label: "Webhook", Icon: "↗", URL: true,
		Lead:        "Kendi sunucuna imzalı JSON.",
		Help:        "Her olay bu HTTPS adresine POST edilir. İstekler X-Paas-Signature başlığıyla imzalanır; imza anahtarı kanal oluşturulunca bir kez gösterilir.",
		Placeholder: "https://ornek.com/paas-webhook"},
	{Kind: store.ChannelEmail, Label: "E-posta", Icon: "✉",
		Lead: "Seçtiğin adreslere e-posta.",
		Help: "Adresleri virgülle ayır. İstersen e-posta adresini tercihlerine kaydetmiş ekip üyelerine de gönderebilirsin."},
}

// eventInfo is one subscribable event with a plain description.
type eventInfo struct {
	Type, Label, Desc string
}

// eventGroup groups events on the notification forms.
type eventGroup struct {
	Title  string
	Events []eventInfo
}

var eventGroups = []eventGroup{
	{"Deploy'lar", []eventInfo{
		{notify.DeploymentReady, notify.EventLabels[notify.DeploymentReady], "Her başarılı deploy; yoğun ekiplerde çok sık gelir."},
		{notify.DeploymentFailed, notify.EventLabels[notify.DeploymentFailed], "Build ya da başlatma hata verdi."},
		{notify.DeploymentCanceled, notify.EventLabels[notify.DeploymentCanceled], "Bir deploy elle ya da yeni bir push ile durduruldu."},
	}},
	{"Production", []eventInfo{
		{notify.ProductionChanged, notify.EventLabels[notify.ProductionChanged], "Canlı site başka bir deploy'u göstermeye başladı."},
		{notify.RolloutRolledBack, notify.EventLabels[notify.RolloutRolledBack], "Kademeli yayın hata oranı yüzünden eski sürüme döndü."},
	}},
	{"Altyapı", []eventInfo{
		{notify.AddonFailed, notify.EventLabels[notify.AddonFailed], "Bir veritabanı hazırlanamadı ya da yedek/geri yükleme başarısız."},
		{notify.DomainFailed, notify.EventLabels[notify.DomainFailed], "Bir alan adının DNS kaydı bulunamadı ya da kayboldu."},
		{notify.CertExpiring, notify.EventLabels[notify.CertExpiring], "Bir alan adının TLS sertifikası yakında sona erecek."},
	}},
	{"Plan", []eventInfo{
		{notify.LimitReached, notify.EventLabels[notify.LimitReached], "Ekip bir plan limitine ulaştı; yeni işlemler reddediliyor."},
	}},
}

// eventChoice is one checkbox of an event fieldset.
type eventChoice struct {
	eventInfo
	Checked bool
}

type eventChoiceGroup struct {
	Title  string
	Events []eventChoice
}

// eventChoices returns the grouped event checkboxes, checked per checked.
func eventChoices(checked func(string) bool) []eventChoiceGroup {
	out := make([]eventChoiceGroup, 0, len(eventGroups))
	for _, g := range eventGroups {
		cg := eventChoiceGroup{Title: g.Title}
		for _, e := range g.Events {
			cg.Events = append(cg.Events, eventChoice{eventInfo: e, Checked: checked(e.Type)})
		}
		out = append(out, cg)
	}
	return out
}

// usageAtLimit says what happens when a usage bar is full.
var usageAtLimit = map[string]string{
	"projects":      "Yeni proje oluşturulamaz; mevcut projeler çalışmaya devam eder.",
	"build_minutes": "Yeni build başlamaz; çalışan siteler etkilenmez.",
	"deploys":       "Yeni deploy kuyruğa alınmaz; canlı site etkilenmez.",
	"addons":        "Yeni veritabanı eklenemez.",
	"domains":       "Yeni alan adı eklenemez.",
	"builds":        "Yeni build'ler sırada bekler; diğer ekipleri engellemez.",
}
