# paas — Faz Planı

Vercel tarzı, Git tabanlı PaaS. Her commit değişmez bir deploy olur ve kendi
URL'sini alır; production yalnızca bir takma addır (alias). Rollback, alias'ı eski
deploy'a çevirmek demektir.

**Hedef ortam:** AWS EC2 (ARM, t4g.medium) + k3s + Traefik + cert-manager (Route 53 DNS-01)
**Dil:** Go · **Durum:** PostgreSQL

## Temel kavramlar

| Kavram | Anlamı |
|---|---|
| App | Bir GitHub reposuna bağlı uygulama (ör. `blog`) |
| Deployment | Belirli bir commit'in değişmez sürümü. URL: `<sha7>-<app>.<domain>` |
| Alias | Bir deploy'u gösteren isim. `<app>.<domain>` (production), `<branch>-<app>.<domain>` (preview) |
| Rollback | Production alias'ını eski bir deploy'a çevirmek. Yeniden build yok. |

## Durum makinesi (Deployment)

```
queued → building → deploying → ready
   ↘             ↘            ↘
    canceled      failed       failed
                  canceled     canceled      (Faz 17: kullanıcı iptali)
```

---

## Faz 0 — Hazırlık ✅
- Repo, Makefile, yerel geliştirme ortamı (Postgres, k3d)
- Mimari diyagram (C4: context + container)
- **Çıktı:** `make dev` ile ayağa kalkan boş servis

## Faz 1 — Kontrol düzlemi: API + webhook + kuyruk ✅
- Go HTTP API (stdlib `net/http`), yapılandırma, graceful shutdown
- PostgreSQL şeması: `apps`, `deployments`, `aliases`, `deployment_logs`
- GitHub webhook alıcısı: HMAC-SHA256 imza doğrulaması, `push` olayı → `queued` deploy
- Postgres tabanlı iş kuyruğu (`FOR UPDATE SKIP LOCKED`), worker iskeleti
- **Çıktı:** Push yapıldığında veritabanında deploy kaydı oluşuyor ve worker onu alıyor

## Faz 2 — Build hattı ⚠️ en riskli faz ✅
- Repoyu commit SHA'sıyla klonlama (shallow clone)
- Dil algılama (Node / Go / statik) + otomatik Dockerfile üretimi
- BuildKit (rootless `buildkitd`) ile build, cache mount'lar
- İmajı registry'ye gönderme: `ECR` veya `ghcr.io` → `app:<sha>`
- Build loglarını satır satır `deployment_logs` tablosuna yazma
- **Çıktı:** Push → arm64 imaj registry'de

## Faz 3 — Deploy katmanı ✅
- Kubernetes API ile kaynak oluşturma: uygulama başına namespace,
  deploy başına `Deployment` + `Service` (`d-<sha7>`)
- CPU/RAM limitleri, `ResourceQuota`, readiness probe
- Deploy hazır olana kadar bekleme, zaman aşımında `failed`
- Ortam değişkenleri → `Secret`
- **Çıktı:** Her commit kümede ayrı bir sürüm olarak çalışıyor

## Faz 4 — Yönlendirme, alias ve TLS ✅
- Traefik üzerinden standart `Ingress`: deploy URL'si + alias URL'leri (taşınabilirlik için CRD yerine)
- Alias'lar veritabanından senkronlanır; periyodik uzlaştırma döngüsü
- Wildcard DNS (`*.domain` → EC2 Elastic IP) — Route 53
- cert-manager + Let's Encrypt **DNS-01** ile wildcard sertifika
- main branch → production alias, diğer branch'ler → preview alias
- Rollback API'si: alias'ı başka bir deploy'a çevir (anında)
- **Çıktı:** `https://blog.domain` ve `https://a3f9c1-blog.domain` çalışıyor

## Faz 5 — Gözlemlenebilirlik ve geliştirici deneyimi ✅
- SSE ile canlı build logu (Postgres `LISTEN/NOTIFY`) ve çalışma zamanı logu (K8s log API)
- GitHub entegrasyonu: commit status + PR'a "Preview hazır" yorumu
- Web arayüzü (Go şablonları + htmx): app listesi, deploy geçmişi, rollback butonu
- **Çıktı:** Demo edilebilir uçtan uca akış

## Faz 6 — Altyapı kodu ve canlıya alma ✅ (kod hazır ve doğrulandı; henüz `apply` edilmedi)
- Terraform: VPC/SG, EC2 (t4g.medium), Elastic IP, Route 53 kayıtları, ECR, IAM
- k3s kurulumu (cloud-init), Traefik + cert-manager Helm kurulumu
- Kontrol düzleminin kendisini kümeye deploy etme
- **Çıktı:** `terraform apply` ile sıfırdan ayağa kalkan platform

## Faz 7 — Sağlamlaştırma ✅
- Eski preview'leri temizleme (PR kapanınca / N gün sonra)
- k6 ile yük testi: deploy süresi, eşzamanlı build, istek/saniye
- Arıza senaryoları: worker çökmesi, build hatası, pod crash loop
- (Bonus) Scale-to-zero: KEDA HTTP add-on

## Faz 8 — Dokümantasyon ve sunum ✅ (demo videosu: senaryo hazır, kayıt bekliyor)
- README, mimari diyagram, demo videosu
- Ölçüm sonuçları (tablolar/grafikler), teknik rapor
- Canlı demo provası

---

## Kapsam genişlemesi (Faz 9 ve sonrası)

İlk sürümün ardından, birbirinden bağımsız ilerleyebilen fazlar. Migration numaraları
çakışmasın diye önceden ayrıldı.

### Faz 9 — Daha fazla dil ✅
- Python (`requirements.txt` / `pyproject.toml`; gunicorn / uvicorn / Django / Flask / FastAPI algılama)
- Ruby (`Gemfile`; Rails / Rack), Java (Maven / Gradle → JRE), hepsi sayısal root olmayan kullanıcıyla
- Her dil için örnek uygulama ve gerçek build testi
- **Çıktı:** `examples/` altındaki yeni uygulamalar Dockerfile'sız build edilip çalışıyor

### Faz 10 — Ortam değişkenlerinin şifrelenmesi (migration 005) ✅
- AES-256-GCM ile şifreleme; anahtar `PAAS_ENV_KEY`, anahtar kimliğiyle rotasyon desteği
- Mevcut düz metin değerlerin şifrelenmesi, anahtarı döndürme komutu
- **Çıktı:** veritabanı dökümünde hiçbir env değeri okunamıyor

### Faz 11 — Sıfıra ölçekleme (migration 008) ✅ (k3d'de doğrulandı)
- Belirli süre istek almayan preview deploy'ları 0 replikaya iner
- İlk istekte uyandırma: istek bekletilir, pod hazır olunca iletilir
- Production için isteğe bağlı (uygulama başına ayar)
- **Çıktı:** boştaki preview'ler kaynak tüketmiyor; ilk istek birkaç saniyede yanıtlanıyor

### Faz 12 — Özel alan adları (migration 006) ✅ (k3d'de doğrulandı; gerçek Let's Encrypt sertifikası AWS'de denenecek)
- Uygulamaya alan adı ekleme (API + arayüz), DNS doğrulaması (CNAME / TXT)
- Alan adı başına Let's Encrypt sertifikası (cert-manager, HTTP-01), production alias'ını izler
- **Çıktı:** `https://www.ornek.com` production deploy'unu gösteriyor, rollback'i izliyor

### Faz 13 — Kullanıcılar ve ekipler (migration 007) ✅
- Kullanıcılar, ekipler, roller (owner / member / viewer); uygulamalar ekibe ait
- GitHub OAuth ile web girişi; kullanıcı başına API token'ları
- Her API ucu ve arayüz sayfası yetki kontrolünden geçer
- **Çıktı:** bir ekip yalnızca kendi uygulamalarını görür ve yönetir

### Faz 14 — Daha hızlı ılık build ✅ (ılık build %18–34 kısa, bkz. MEASUREMENTS §7)
- Dockerfile frontend'inin sabitlenmesi / kaldırılması (her build'deki ~2 s'lik sabit maliyet)
- **Çıktı:** değişiklik olmayan build'ler belirgin şekilde kısa

### Faz 15 — GitHub App (migration 009) ✅ (sahte GitHub'a karşı test edildi; gerçek App ile deneme bekliyor)
- Kişisel token, OAuth App ve repo başına webhook yerine tek bir GitHub App
- Kurulum olayları (`installation`, `installation_repositories`) veritabanına; kısa ömürlü kurulum token'larıyla clone, commit status ve PR yorumu
- "Install" → repoları seç → arayüzde listeden "Import": uygulama oluşur, ilk deploy başlar
- GitHub ile giriş aynı App üzerinden; eski token + webhook yolu yedek olarak çalışır
- **Çıktı:** yeni bir proje eklemek GitHub'da hiçbir ayar gerektirmiyor

### Faz 16 — Proje ayarları ve framework presetleri (migration 010) ✅
- Uygulama başına build ayarları (Vercel'in "Build & Development Settings"i): kök dizin (monorepo),
  framework seçimi, install / build / start komutları, çıktı dizini, Node sürümü; API `GET/PUT /api/apps/{name}/settings`
- Framework preset'leri: Next.js (standalone / export / `next start`), Vite, Create React App, Astro (statik / node),
  SvelteKit (node / static adapter), Nuxt (SSR / generate), Remix, React Router, NestJS, Express
- Algılanan framework build logunda (`framework: Next.js`), deployment kaydında ve ayarlarda
- Next.js ve Vite örnekleri gerçek build testinde
- **Çıktı:** monorepo'daki bir Next.js uygulaması tek ayarla, Dockerfile'sız deploy ediliyor

### Faz 17 — Ortamlar ve deploy kontrolleri (migration 011) ✅ (Postgres'e karşı test edildi; kümede deneme bekliyor)
- Ortama özel değişkenler: `production` / `preview` / `all`, preview için isteğe bağlı branch; en özel olan kazanır.
  Eski satırlar `all` oldu ve şifreleme AAD'si değişmediği için yeniden yazılmadı; yeni hedeflerde AAD hedef + branch içerir
- Promote: hazır preview, aynı imajla (build yok) production değişkenleriyle yeni bir deploy olarak production'a geçer
- Redeploy: aynı commit, varsayılan imajı yeniden kullanır; `use_cache: false` yeniden build eder.
  Bir commit'in kopyaları nesil numarası alır (`d-<sha7>-<n>`, `<sha7>-<n>-<app>`)
- İptal: yeni `canceled` durumu; kuyruktaki hemen, çalışan heartbeat döngüsü üzerinden context iptaliyle
- Deploy hook'ları: uygulama başına gizli URL (token'ın SHA-256'sı saklanır), branch'in son commit'ini deploy eder
- Build'i atlama: head commit mesajında `[skip deploy]` / `[skip ci]` olan push'lar deploy edilmez
- **Çıktı:** Vercel'deki ortam / promote / redeploy / cancel / deploy hook akışları API'den kullanılabiliyor

### Faz 18 — `paas` komut satırı aracı ✅ (sahte API sunucusuna karşı test edildi)
- `cmd/paas-cli` (ikili adı `paas`) ve test edilebilir `internal/cli` paketi; yalnızca standart kütüphane
- `paas login` kişisel API token'ını `GET /api/me` ile doğrular, URL ve token'ı `os.UserConfigDir()/paas/config.json`'a (`0600`) yazar; `logout`, `whoami`
- `ls`, `deployments`, `inspect`; `logs` (sayfalı), `logs -f` (SSE, kopan bağlantıda kaldığı satırdan devam, bitişte çıkış kodu), `logs --runtime` (pod logları)
- `rollback`, `env ls|set|rm`, `domains ls|add|verify|rm`, `import`, `open`
- Ortak bayraklar `--url`, `--token`, `--json` (veya `PAAS_URL` / `PAAS_TOKEN`); tablolar, göreli zamanlar, çıkış kodları 0 / 1 / 2, 401 / 403 / 404 için ipuçlu hatalar
- **Çıktı:** web arayüzünde yapılan her günlük işlem terminalden ve CI'dan da yapılabiliyor

### Faz 19 — Analitik ve gözlemlenebilirlik (migration 012) ✅ (birim ve veritabanı testleriyle doğrulandı; canlı kümede deneme bekliyor)
- Traefik'in servis başına sayaçlarından deploy başına dakikalık istek metrikleri: 2xx/3xx/4xx/5xx, yanıt süresi histogramı; sayaç sıfırlanmalarına dayanıklı fark hesabı, 7 günlük saklama
- `HelmChartConfig` (`infra/k8s/05-traefik-config.yaml`) ile Traefik'te servis / entrypoint etiketleri ve ince histogram kovaları; sıfıra ölçekleme de buna dayanır
- `GET /api/apps/{name}/analytics?range=1h|24h|7d`: zaman serisi, toplamlar, p50/p95, deploy dağılımı
- `GET /api/apps/{name}/health`: deploy başına son 15 dakikanın 5xx oranı; `GET /api/apps/{name}/usage`: metrics-server'dan canlı CPU / bellek
- **Çıktı:** bir uygulamanın trafiği, hataları, gecikmesi ve kaynak kullanımı ek bir izleme yığını olmadan API'den okunuyor

### Arayüz — Faz 16–19 ekranları: proje ayarları, ortamlar, deploy kontrolleri ve analitik ✅ (Postgres'e karşı test edildi)
- Proje sayfası sunucuda çizilen sekmelere ayrıldı: Genel (`/apps/{name}`), Deploy'lar, Analitik, Ayarlar
- Deploy listesi: deploy'un kendi adresi (`Deployment.Host`, nesil ekiyle; önceden commit'ten türetiliyordu), framework,
  ortam rozeti, kaynak (redeploy / promote / hook); member+ için "Production'a taşı", "Yeniden deploy et"
  ("Önbelleği kullanma"), "İptal et"; yeni "İptal edildi" rozeti
- Ayarlar: build ayarları formu (`build.Frameworks`, "Otomatik algıla (<algılanan>)", `build.NormalizeSettings` hataları
  Türkçe), hedefli ortam değişkenleri (Tümü / Production / Önizleme + branch), deploy hook'ları (URL bir kez gösterilir),
  alan adları ve uyku modu
- Analitik: 1 sa / 24 sa / 7 gün, toplamlar (istek, hata oranı, p95), JavaScript'siz SVG grafik (açık / koyu temada
  doğrulanmış renkler), deploy sağlığı, canlı CPU / bellek; kullanım API'si yoksa "Kullanım verisi yok"
- API'deki iş mantığı dışa açık yardımcılara taşındı (`api.Promote`, `Redeploy`, `Cancel`, `NormalizeHook`, `CreateHook`,
  `CheckEnvScope`, `Analytics`, `Health`, `Usage`); JSON API ve arayüz aynı kodu çalıştırır, davranış değişmedi
- Her form CSRF + aynı-origin kontrolünden geçer, izleyicinin POST'u 403 alır; testler her form, doğrulama hatası,
  rol / duruma göre düğme görünürlüğü ve örnek metriklerle analitik sayfası için
- **Çıktı:** Faz 16–19'da API'ye eklenen her şey tarayıcıdan da kullanılabiliyor

## Sonraki aşamalar
Veritabanı sağlama, faturalandırma, çok düğümlü küme.
