package web

import "github.com/nisagwn/paas/internal/api"

// Faz 28: Turkish texts of project deletion. They extend the tables of
// tr.go here, like tr_plans.go.

const (
	// msgDeleteConfirm: the typed name does not match (%s: the project name).
	msgDeleteConfirm = "Onaylamak için proje adını tam olarak yaz: %s"
	// msgDeleteOwner explains the disabled button to members and viewers.
	msgDeleteOwner = "Projeyi yalnızca ekip sahipleri silebilir. Silinmesi gerekiyorsa ekibin sahibine yaz."
	// flashDeleted / flashDeleting: the dashboard after a deletion (%s: the
	// project name). The second one when running deploys are still
	// stopping or the cluster is still removing the namespace.
	flashDeleted  = "„%s“ projesi silindi."
	flashDeleting = "„%s“ projesi silindi. Kalan kaynaklar arka planda kaldırılıyor; aynı ad birkaç dakika içinde yeniden kullanılabilir."
)

var deleteMessages = map[string]string{
	api.MsgAppDeleting: "Bu adla bir proje hâlâ siliniyor. Bir dakika sonra tekrar dene.",
}

func init() {
	for k, v := range deleteMessages {
		messages[k] = v
		normMessages[norm(k)] = v
	}
}
