# minipaas — Faz Planı

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

## Faz 0 — Hazırlık
- Repo, Makefile, yerel geliştirme ortamı (Postgres, k3d)
- Mimari diyagram (C4: context + container)
- **Çıktı:** `make dev` ile ayağa kalkan boş servis

## Faz 1 — Kontrol düzlemi: API + webhook + kuyruk
- Go HTTP API (stdlib `net/http`), yapılandırma, graceful shutdown
- PostgreSQL şeması: `apps`, `deployments`, `aliases`, `deployment_logs`
- GitHub webhook alıcısı: HMAC-SHA256 imza doğrulaması, `push` olayı → `queued` deploy
- Postgres tabanlı iş kuyruğu (`FOR UPDATE SKIP LOCKED`), worker iskeleti
- **Çıktı:** Push yapıldığında veritabanında deploy kaydı oluşuyor ve worker onu alıyor

## Faz 2 — Build hattı ⚠️ en riskli faz
- Repoyu commit SHA'sıyla klonlama (shallow clone)
- Dil algılama (Node / Go / statik) + otomatik Dockerfile üretimi
- BuildKit (rootless `buildkitd`) ile build, cache mount'lar
- İmajı registry'ye gönderme: `ECR` veya `ghcr.io` → `app:<sha>`
- Build loglarını satır satır `deployment_logs` tablosuna yazma
- **Çıktı:** Push → arm64 imaj registry'de

## Faz 3 — Deploy katmanı
- Kubernetes API ile kaynak oluşturma: uygulama başına namespace,
  deploy başına `Deployment` + `Service` (`d-<sha7>`)
- CPU/RAM limitleri, `ResourceQuota`, readiness probe
- Deploy hazır olana kadar bekleme, zaman aşımında `failed`
- Ortam değişkenleri → `Secret`
- **Çıktı:** Her commit kümede ayrı bir sürüm olarak çalışıyor

## Faz 4 — Yönlendirme, alias ve TLS
- Traefik üzerinden standart `Ingress`: deploy URL'si + alias URL'leri (taşınabilirlik için CRD yerine)
- Alias'lar veritabanından senkronlanır; periyodik uzlaştırma döngüsü
- Wildcard DNS (`*.domain` → EC2 Elastic IP) — Route 53
- cert-manager + Let's Encrypt **DNS-01** ile wildcard sertifika
- main branch → production alias, diğer branch'ler → preview alias
- Rollback API'si: alias'ı başka bir deploy'a çevir (anında)
- **Çıktı:** `https://blog.domain` ve `https://a3f9c1-blog.domain` çalışıyor

## Faz 5 — Gözlemlenebilirlik ve geliştirici deneyimi
- SSE ile canlı build logu (Postgres `LISTEN/NOTIFY`) ve çalışma zamanı logu (K8s log API)
- GitHub entegrasyonu: commit status + PR'a "Preview hazır" yorumu
- Web arayüzü (Go şablonları + htmx): app listesi, deploy geçmişi, rollback butonu
- **Çıktı:** Demo edilebilir uçtan uca akış

## Faz 6 — Altyapı kodu ve canlıya alma
- Terraform: VPC/SG, EC2 (t4g.medium), Elastic IP, Route 53 kayıtları, ECR, IAM
- k3s kurulumu (cloud-init), Traefik + cert-manager Helm kurulumu
- Kontrol düzleminin kendisini kümeye deploy etme
- **Çıktı:** `terraform apply` ile sıfırdan ayağa kalkan platform

## Faz 7 — Sağlamlaştırma
- Eski preview'leri temizleme (PR kapanınca / N gün sonra)
- k6 ile yük testi: deploy süresi, eşzamanlı build, istek/saniye
- Arıza senaryoları: worker çökmesi, build hatası, pod crash loop
- (Bonus) Scale-to-zero: KEDA HTTP add-on

## Faz 8 — Dokümantasyon ve sunum
- README, mimari diyagram, demo videosu
- Ölçüm sonuçları (tablolar/grafikler), teknik rapor
- Canlı demo provası

---

## Sonraki aşamalar (kapsam genişlemesi)
İlk sürümün ardından eklenecek alanlar: multi-tenancy ve kullanıcı yönetimi,
veritabanı sağlama, özel alan adları, faturalandırma, daha fazla dil ve
framework için otomatik build desteği, çok düğümlü küme.
