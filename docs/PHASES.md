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
                 ↘            ↘
                  failed       failed
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

## Kapsam genişlemesi (Faz 9–16)

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

## Sonraki aşamalar
Veritabanı sağlama, faturalandırma, çok düğümlü küme.
