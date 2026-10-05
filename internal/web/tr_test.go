package web

import (
	"testing"
	"time"
)

func TestTranslate(t *testing.T) {
	for in, want := range map[string]string{
		"Team not found.":                                 "Ekip bulunamadı.",
		"team not found":                                  "Ekip bulunamadı.",
		"IP addresses are not domain names":               "IP adresi alan adı değildir.",
		`Repo must look like "owner/repo".`:               `Repo "sahip/repo" biçiminde olmalı.`,
		"PORT is set by the platform":                     "PORT platform tarafından ayarlanır.",
		"This repository is already deployed as app web.": "Bu repo zaten web projesi olarak ekli.",
		"unknown text stays":                              "unknown text stays",
		"":                                                "",
	} {
		if got := tr(in); got != want {
			t.Errorf("tr(%q) = %q, want %q", in, got, want)
		}
	}
	if statusLabel("ready") != "Hazır" || statusLabel("weird") != "weird" || roleLabel("owner") != "Sahip" {
		t.Error("labels")
	}
}

func TestTurkishTimes(t *testing.T) {
	if got := ago(time.Now().Add(-3 * time.Minute)); got != "3 dk önce" {
		t.Errorf("ago = %q", got)
	}
	for d, want := range map[time.Duration]string{
		12 * time.Second:              "12 sn",
		3*time.Minute + 5*time.Second: "3 dk 5 sn",
		2 * time.Minute:               "2 dk",
		time.Hour + 2*time.Minute:     "1 sa 2 dk",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
