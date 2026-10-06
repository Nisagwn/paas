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

// Faz 16–19 screens: errors of build settings, environments and deploy controls, and
// the number formats of the Analitik tab.
func TestTranslateF20(t *testing.T) {
	for in, want := range map[string]string{
		"install_command is longer than 1024 characters":                    "Install komutu en fazla 1024 karakter olabilir.",
		`root_directory must be relative (no leading "/")`:                  `Kök dizin göreli olmalı (başta "/" olmadan).`,
		"output_directory may only contain letters, digits and . _ @ + -":   "Çıktı dizini yalnızca harf, rakam ve . _ @ + - içerebilir.",
		"start_command must not end with a backslash":                       `Start komutu "\" ile bitemez.`,
		"only ready deployments can be promoted (status failed)":            "Yalnızca hazır deploy'lar production'a taşınabilir (durum: Başarısız).",
		"deployment is still building; cancel it or wait until it finishes": "Deploy hâlâ sürüyor (Kuruluyor); iptal et ya da bitmesini bekle.",
		`git_branch needs target "preview"`:                                 "Branch yalnızca Önizleme ortamı için seçilebilir.",
	} {
		if got := tr(in); got != want {
			t.Errorf("tr(%q) = %q, want %q", in, got, want)
		}
	}
	if statusLabel("canceled") != "İptal edildi" || healthLabel("degraded") != "Sorunlu" || targetLabel("preview") != "Önizleme" {
		t.Error("labels")
	}
	ms := func(f float64) *float64 { return &f }
	for got, want := range map[string]string{
		number(1234567): "1.234.567", number(12): "12", pct(0.0134): "%1,3",
		millis(nil): "—", millis(ms(550)): "550 ms", millis(ms(1500)): "1,5 sn", millis(ms(2.5)): "2,5 ms",
		byteSize(64 << 20): "64 MiB", byteSize(3 << 29): "1,5 GiB", cpuLabel(250): "250m", cpuLabel(1500): "1,5 çekirdek",
	} {
		if got != want {
			t.Errorf("format: %q, want %q", got, want)
		}
	}
	for max, want := range map[int64]int64{1: 1, 3: 5, 7: 10, 12: 20, 1000: 1000, 1001: 2000} {
		if got := niceCeil(max); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", max, got, want)
		}
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
