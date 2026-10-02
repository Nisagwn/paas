# minipaas

Vercel tarzı, Git tabanlı mini PaaS. Bitirme projesi.

Her `git push` değişmez bir **deployment** üretir ve kendi URL'sini alır.
`production` yalnızca bir takma addır (alias); rollback, alias'ı eski bir
deployment'a çevirmektir ve yeniden build gerektirmez.

```
git push ──► GitHub webhook ──► API ──► Postgres kuyruğu ──► worker
                                                         │
                         build (BuildKit) ◄──────────────┤   Faz 2
                         deploy (Kubernetes) ◄───────────┤   Faz 3
                         alias + TLS (Traefik) ◄─────────┘   Faz 4
```

| URL | Ne gösterir |
|---|---|
| `https://<sha7>-<app>.<domain>` | Tek bir commit'in deployment'ı (asla değişmez) |
| `https://<branch>-<app>.<domain>` | Branch'in son başarılı deployment'ı (preview) |
| `https://<app>.<domain>` | Production (production branch'in son deployment'ı veya rollback hedefi) |

Faz planı: [docs/PHASES.md](docs/PHASES.md)

## Durum: Faz 1 tamamlandı

- [x] Go HTTP API (stdlib `net/http`), bearer token ile korunuyor
- [x] PostgreSQL şeması + gömülü migration'lar (advisory lock ile)
- [x] GitHub webhook: HMAC-SHA256 imza doğrulaması, `push` → kuyruğa deployment
- [x] Postgres tabanlı iş kuyruğu (`FOR UPDATE SKIP LOCKED`), eşzamanlı worker'lar
- [x] Durum makinesi: `queued → building → deploying → ready | failed`, zaman aşımı
- [x] Production / preview alias'ları, anında rollback
- [x] Deployment logları (artımlı okuma: `?after=<id>`)
- [x] Dry-run pipeline (Faz 2 ve 3 gelene kadar aşamaları taklit eder)
- [ ] Faz 2: gerçek build (BuildKit + registry)

## Yerel geliştirme

Gerekenler: Go 1.24+, Docker, `openssl`.

```bash
cp .env.example .env
make db        # Postgres'i Docker'da başlatır
make test      # tüm testler (veritabanı entegrasyon testleri dahil)
make run       # API + worker  →  http://localhost:8080
```

Başka bir terminalde:

```bash
source .env
H="Authorization: Bearer $MINIPAAS_API_TOKEN"

# 1. Uygulama oluştur
curl -s -H "$H" -d '{"name":"blog","repo":"nisagwn/blog"}' localhost:8080/api/apps

# 2. GitHub push'unu taklit et (imzalı webhook)
scripts/push.sh nisagwn/blog main
scripts/push.sh nisagwn/blog feature/dark-mode

# 3. Deployment'lar ve alias'lar
curl -s -H "$H" localhost:8080/api/apps/blog/deployments
curl -s -H "$H" localhost:8080/api/apps/blog

# 4. Rollback
curl -s -H "$H" -d '{"deployment_id":1}' localhost:8080/api/apps/blog/rollback

# 5. Loglar
curl -s -H "$H" localhost:8080/api/deployments/1/logs
```

### Gerçek GitHub'a bağlamak

Repo → Settings → Webhooks → Add webhook:
- **Payload URL:** `https://<sunucun>/webhooks/github` (yerelde test için ngrok veya Cloudflare Tunnel)
- **Content type:** `application/json`
- **Secret:** `MINIPAAS_GITHUB_WEBHOOK_SECRET` ile aynı
- **Events:** Just the push event

## API

Tüm `/api/*` uçları `Authorization: Bearer <MINIPAAS_API_TOKEN>` ister.

| Yöntem | Yol | Açıklama |
|---|---|---|
| GET | `/healthz` | Sağlık kontrolü (veritabanına ping atar) |
| POST | `/webhooks/github` | GitHub webhook alıcısı (imza ile korunur) |
| POST | `/api/apps` | `{"name","repo","production_branch"?}` |
| GET | `/api/apps` | Uygulamalar |
| GET | `/api/apps/{name}` | Uygulama + alias'lar |
| GET | `/api/apps/{name}/deployments?limit=` | Deployment geçmişi |
| POST | `/api/apps/{name}/rollback` | `{"deployment_id"}` — production alias'ını taşır |
| GET | `/api/deployments/{id}` | Tek deployment |
| GET | `/api/deployments/{id}/logs?after=` | Log satırları |

## Proje yapısı

```
cmd/minipaas/         giriş noktası: API + worker tek süreçte, graceful shutdown
internal/config/      ortam değişkenleri
internal/store/       PostgreSQL erişimi, migration'lar, kuyruk
internal/webhook/     GitHub imza doğrulama ve push ayrıştırma
internal/api/         HTTP uçları
internal/worker/      kuyruk tüketicisi, Pipeline arayüzü, dry-run pipeline
internal/naming/      DNS ve Kubernetes için güvenli isimler
internal/testdb/      testler için temiz veritabanı
```

## Tasarım kararları

- **Kuyruk olarak Postgres:** Ayrı bir Redis/RabbitMQ yerine `SKIP LOCKED`.
  Daha az parça, işlemsel tutarlılık. Test: 8 worker × 50 iş, her iş tam bir kez alınır.
- **Değişmez deployment'lar:** `(app_id, commit_sha)` benzersiz. Aynı commit'in
  tekrar gelmesi (GitHub yeniden gönderimi) yeni deployment üretmez.
- **Alias sadece ileri gider:** Eski bir commit'in build'i geç biterse, daha yeni
  bir deployment'ın production'ını ezmez.
- **Tek label'lı hostname'ler:** Hepsi `*.domain` altında olduğu için tek bir
  wildcard sertifika (DNS-01) hepsini kapsar.
- **Sadece stdlib + lib/pq:** Bağımlılık sayısı bilinçli olarak düşük.
