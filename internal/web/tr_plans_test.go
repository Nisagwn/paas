package web

import (
	"strings"
	"testing"
)

// Every refusal of internal/plans and the Faz 23 API reaches the UI in
// Turkish.
func TestTranslatePlans(t *testing.T) {
	for in, want := range map[string]string{
		"plan limit reached: the Hobby plan allows 10 projects per team":                                           "Hobby planında ekip başına en fazla 10 proje",
		"plan limit reached: the Pro plan allows 1000 deployments per day":                                         "Pro planında günde en fazla 1000 deploy",
		"plan limit reached: the Hobby plan allows 600 build minutes per month":                                    "aylık 600 build dakikası bu ay doldu",
		"plan limit reached: the Hobby plan allows 3 custom domains per team":                                      "en fazla 3 alan adı",
		"plan limit reached: the Hobby plan allows 2 databases per team":                                           "en fazla 2 veritabanı",
		"plan limit reached: the Hobby plan allows 2 replicas per process":                                         "süreç başına en fazla 2 kopya",
		"plan limit reached: the Hobby plan allows 2 cron jobs; paas.yaml declares 3":                              "en fazla 2 cron işi olabilir; paas.yaml 3 tane",
		"plan limit reached: the Hobby plan allows 2 replicas per process; paas.yaml asks for 4 replicas of queue": "paas.yaml 4 kopya istiyor (queue)",
		"the team is suspended: deployments and new resources are disabled":                                        "Ekip askıya alındı",
		"the team is suspended: deployments and new resources are disabled (spam)":                                 "kapalı (spam).",
		"build: the build exceeded the plan's build timeout of 15m0s":                                              "planın 15m0s olan build süre sınırını",
		"your account is suspended":                                    "Hesabın askıya alındı.",
		"too many requests; try again later":                           "Çok fazla istek.",
		"too many new projects; try again later":                       "çok fazla proje",
		`plan must be "hobby", "pro" or "unlimited"`:                   "Plan Hobby, Pro ya da Sınırsız olmalı.",
		`limit projects must be a non-negative integer or "unlimited"`: "projects limiti negatif olmayan",
		`unknown limit "gpus"`:                                         "Bilinmeyen limit: gpus.",
	} {
		if got := tr(in); !strings.Contains(got, want) || got == in {
			t.Errorf("tr(%q) = %q, want it to contain %q", in, got, want)
		}
	}
	if planLabel("unlimited") != "Sınırsız" || planLabel("hobby") != "Hobby" {
		t.Error("plan labels")
	}
}
