package web

import (
	"fmt"
	"net/url"
	"time"
)

// Turkish texts of the sign-in and sign-up pages (login.go, signup.go).
// They extend loginErrors of oauth.go here, so other phases editing that
// file do not collide.

// authHelp is the "what now" line under a sign-in error.
type authHelp struct {
	Text string
	Link string // empty: no link
	Href string
}

func init() {
	loginErrors["state"] = "Giriş denemesinin süresi doldu ya da bu tarayıcıda başlatılmadı."
	loginErrors["not_allowed"] = "Bu GitHub hesabının giriş izni yok."
	loginErrors["bad_token"] = "Geçersiz API token'ı."
}

// loginHelp tells what to do after the sign-in error code; nil when there
// is nothing to add.
func (s *Server) loginHelp(code, next string) *authHelp {
	retry := "/auth/github?next=" + url.QueryEscape(next)
	switch code {
	case "not_allowed":
		return &authHelp{
			Text: "Kayıtlar şu an davetle. Bir ekip sahibi GitHub kullanıcı adını ekibine ekledikten sonra aynı hesapla tekrar giriş yap.",
			Link: "Davet nasıl işler", Href: "/signup",
		}
	case "suspended":
		h := &authHelp{Text: "Askı kaldırılınca aynı GitHub hesabıyla tekrar giriş yapabilirsin."}
		if s.Legal.ContactEmail != "" {
			h.Link, h.Href = "Platform yöneticisine yaz", "mailto:"+s.Legal.ContactEmail
		} else {
			h.Link, h.Href = "Kullanım koşulları", "/terms"
		}
		return h
	case "too_new":
		age := "belirli bir süre"
		if s.GitHub != nil && s.GitHub.MinAccountAge > 0 {
			age = ageLabel(s.GitHub.MinAccountAge)
		}
		return &authHelp{
			Text: fmt.Sprintf("Herkese açık kayıt için GitHub hesabının en az %s önce açılmış olması gerekir. Bir ekip sahibi seni ekibine eklerse beklemeden girebilirsin.", age),
			Link: "Kayıt koşulları", Href: "/signup",
		}
	case "state":
		return s.retryHelp("Giriş bağlantısı 10 dakika geçerlidir ve başladığın tarayıcıda tamamlanmalıdır.", retry)
	case "denied":
		return s.retryHelp("Devam etmek için GitHub'ın izin ekranında Authorize'a bas.", retry)
	case "failed":
		return s.retryHelp("Birkaç dakika sonra tekrar dene; sürerse platform yöneticisine haber ver.", retry)
	case "bad_token":
		return &authHelp{Text: "Token'ı sunucunun yapılandırmasındaki PAAS_API_TOKEN değerinden kopyala."}
	}
	return nil
}

func (s *Server) retryHelp(text, href string) *authHelp {
	h := &authHelp{Text: text}
	if s.GitHub != nil {
		h.Link, h.Href = "Tekrar dene", href
	}
	return h
}

// ageLabel is a minimum account age in days ("30 gün"), or hours below a day.
func ageLabel(d time.Duration) string {
	if days := int(d.Hours() / 24); days >= 1 {
		return fmt.Sprintf("%d gün", days)
	}
	h := int(d.Hours())
	if h < 1 {
		h = 1
	}
	return fmt.Sprintf("%d saat", h)
}
