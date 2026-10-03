# paas

Vercel tarzı, Git tabanlı PaaS.

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

## Durum: Faz 7 tamamlandı

- [x] Go HTTP API (stdlib `net/http`), bearer token ile korunuyor
- [x] PostgreSQL şeması + gömülü migration'lar (advisory lock ile)
- [x] GitHub webhook: HMAC-SHA256 imza doğrulaması, `push` → kuyruğa deployment
- [x] Postgres tabanlı iş kuyruğu (`FOR UPDATE SKIP LOCKED`), eşzamanlı worker'lar
- [x] Durum makinesi: `queued → building → deploying → ready | failed`, zaman aşımı
- [x] Production / preview alias'ları, anında rollback
- [x] Deployment logları (artımlı okuma: `?after=<id>`)
- [x] Dry-run pipeline (gerçek aşamalar hazır olana kadar taklit eder)
- [x] Faz 2: commit SHA'sıyla shallow clone (private repo için token, argv/loglara sızmaz)
- [x] Faz 2: dil algılama (Dockerfile / Node / Go / statik) + cache mount'lu Dockerfile üretimi
- [x] Faz 2: BuildKit (`buildctl` → `buildkitd`) veya yerelde `docker buildx`; registry'ye `<app>:<sha>` push
- [x] Faz 2: imaj digest ile sabitlenir (`…:<sha>@sha256:…`), isteğe bağlı registry layer cache
- [x] Faz 2: build çıktısı satır satır `deployment_logs`'a
- [x] Faz 3: uygulama başına namespace `app-<app>` + `ResourceQuota`, `LimitRange`, ingress `NetworkPolicy`
- [x] Faz 3: commit başına `Deployment` + `Service` `d-<sha7>` (ClusterIP :80 → :8080), CPU/RAM limitleri, readiness probe
- [x] Faz 3: ortam değişkenleri API'si → deploy anında değişmez `Secret` `d-<sha7>-env` (`envFrom`)
- [x] Faz 3: idempotent create-or-update (çökme sonrası tekrar çalıştırma `AlreadyExists` ile düşmez)
- [x] Faz 3: hazır olana kadar bekleme, pod durumları loga; `ImagePullBackOff` / `CrashLoopBackOff` / kota aşımında
  hemen `failed` (crash'te son log satırlarıyla), zaman aşımında `failed`
- [x] Faz 4: her deploy'a kendi `Ingress`'i: `https://<sha7>-<app>.<domain>` (Deployment'a ait, onunla silinir)
- [x] Faz 4: alias `Ingress`'leri (production + branch başına preview) veritabanından senkronlanır
- [x] Faz 4: rollback = alias `Ingress`'inin backend'ini çevirmek; pod yeniden başlamaz, build yok
- [x] Faz 4: wildcard sertifika (cert-manager, DNS-01) Traefik'in varsayılanı; her host otomatik TLS
- [x] Faz 4: periyodik uzlaştırma (reconcile) döngüsü: başarısız bir senkron kendiliğinden düzelir
- [x] Faz 5: GitHub commit status'ları (`pending` → `success` / `failure`) ve PR'da güncellenen "Preview hazır" yorumu
- [x] Faz 5: canlı build logu (SSE; Postgres `LISTEN/NOTIFY`, bağlantı koparsa periyodik okumaya düşer)
- [x] Faz 5: çalışma zamanı logu (pod logları, Kubernetes log API'si)
- [x] Faz 5: web arayüzü (Go şablonları + htmx): uygulamalar, deploy geçmişi, canlı log, rollback, ortam değişkenleri
- [x] Faz 7: saklama politikası: son N production deploy'u rollback için tutulur, alias'sız eski preview'ler emekliye ayrılır
- [x] Faz 7: branch silinince / PR kapanınca preview alias'ı kalkar, deploy'ları emekliye ayrılır
- [x] Faz 7: çökme sonrası kurtarma: worker heartbeat'i, sahipsiz deploy'lar yeniden kuyruğa (en fazla 2 deneme)
- [x] Faz 7: k6 yük testleri ([loadtest/](loadtest/)), arıza senaryoları ([docs/FAILURE-SCENARIOS.md](docs/FAILURE-SCENARIOS.md))
- [ ] Faz 8: dokümantasyon, mimari diyagram, ölçüm sonuçları

### Deploy nasıl çalışır

`PAAS_DEPLOYER=kubernetes` iken worker, build'in ürettiği digest'li imajı kümeye koyar:

```
namespace app-<app>          (managed-by=paas, ResourceQuota + LimitRange + NetworkPolicy "paas")
├── Secret     d-<sha7>-env  değişmez; deploy anındaki ortam değişkenleri
├── Deployment d-<sha7>      1 replika, PORT=8080, PAAS_APP/COMMIT_SHA/BRANCH, envFrom secret
├── Service    d-<sha7>      ClusterIP :80 → :8080
├── Ingress    d-<sha7>      <sha7>-<app>.<domain> → Service d-<sha7>
└── Ingress    alias-<etiket> <app>.<domain> (production), <branch>-<app>.<domain> (preview)
```

### Yönlendirme ve rollback (Faz 4)

Alias'ların nereyi gösterdiğinin tek doğru kaynağı Postgres'teki `aliases` tablosudur.
`internal/routing` bu durumu kümeye yansıtır:

1. Deploy hazır olunca `MarkReady` alias'ları ileri taşır → senkron
2. `POST /api/apps/{name}/rollback` production alias'ını geri çevirir → senkron
3. Her `PAAS_ROUTE_SYNC_INTERVAL`'da (varsayılan 1 dk) tüm uygulamalar uzlaştırılır

Senkron idempotenttir: aynı durum tekrar uygulanınca kümeye hiçbir yazma yapılmaz;
veritabanında olmayan alias `Ingress`'leri silinir. Aynı uygulamanın senkronları sıraya
girer ve her biri veritabanını kilidin içinde okur, böylece geç biten eski bir senkron
yeni bir rollback'i ezemez. Yönlendirme katmanı hata verirse rollback yine kaydedilir,
API `502` ile bildirir ve bir sonraki uzlaştırma uygular.

Standart `networking.k8s.io/v1 Ingress` kullanılır (Traefik CRD'si değil): CRD gerekmez,
client-go ile tipli ve test edilebilir, başka bir ingress controller ile de çalışır.
TLS bölümünde `secretName` olmadığı için Traefik varsayılan (wildcard) sertifikayı sunar.

| Değişken | Açıklama |
|---|---|
| `PAAS_INGRESS_CLASS` | varsayılan `traefik`; boş = kümenin varsayılan sınıfı |
| `PAAS_INGRESS_TLS` | `true` (varsayılan) → `websecure` + TLS; `false` → düz HTTP (yerel küme) |
| `PAAS_ROUTE_SYNC_INTERVAL` | uzlaştırma aralığı, varsayılan `1m` |

- Etiketler: `app=<app>`, `paas/deployment-id`, `paas/commit-sha`, `paas/branch` (slug; tam adı annotation'da).
- Pod güvenliği: `runAsNonRoot` (sabit `runAsUser` yok, imajın sayısal `USER`'ı kullanılır),
  `allowPrivilegeEscalation: false`, tüm capability'ler düşürülür, `RuntimeDefault` seccomp,
  service account token'ı bağlanmaz.
- `Secret` ve `Service` Deployment'a ait (ownerReference): Deployment silinince onlar da silinir.
- `NetworkPolicy`: app pod'larına yalnızca `kube-system` (Traefik) erişebilir; app'ler birbirine ancak public URL'leri üzerinden ulaşır.
- Ortam değişkeni değişikliği yalnızca sonraki deployment'ları etkiler; çalışan deployment'ın snapshot'ı sabittir.

| Değişken | Açıklama |
|---|---|
| `PAAS_DEPLOYER` | `dryrun` · `kubernetes` |
| `PAAS_KUBECONFIG` | boş = pod içindeyse in-cluster, değilse `$KUBECONFIG` / `~/.kube/config` |
| `PAAS_ROLLOUT_TIMEOUT` | hazır olma bekleme süresi (varsayılan `3m`) |
| `PAAS_APP_CPU_REQUEST` / `_LIMIT` | container başına CPU (varsayılan `25m` / `500m`) |
| `PAAS_APP_MEMORY_REQUEST` / `_LIMIT` | container başına bellek (varsayılan `64Mi` / `256Mi`) |
| `PAAS_APP_QUOTA_CPU` | app namespace'i için `requests.cpu` kotası (varsayılan `1`) |
| `PAAS_APP_QUOTA_MEMORY` | `requests.memory` + `limits.memory` kotası (varsayılan `4Gi`) |
| `PAAS_APP_QUOTA_PODS` | pod kotası (varsayılan `20`) |
| `PAAS_APP_RUN_AS_NON_ROOT` | varsayılan `true`; repodaki Dockerfile isimli `USER` (ör. `USER node`) kullanıyorsa kubelet reddeder |

> Her commit ayrı bir Deployment olarak çalışır; kotayı Faz 7'nin temizlik döngüsü korur
> (aşağıda). Kota yine de dolarsa yeni deploy "exceeded quota" hatasıyla hemen `failed` olur.

### Yaşam döngüsü ve temizlik (Faz 7)

Deploy'lar değişmezdir ama sonsuza kadar çalışmaz. Temizlik döngüsü (`PAAS_GC_INTERVAL`,
varsayılan 10 dk; bir deploy bitince de tetiklenir) her uygulama için:

1. Herhangi bir alias'ın gösterdiği deploy'a **asla dokunmaz**.
2. Production branch'inin en yeni `PAAS_KEEP_PRODUCTION` (5) hazır deploy'unu rollback hedefi olarak tutar.
3. Alias'ı kalmamış preview deploy'larını, bitişlerinden `PAAS_PREVIEW_TTL` (72 saat) sonra emekliye ayırır.
4. Emekli ve başarısız deploy'ların Kubernetes nesnelerini siler (Deployment silinince Service,
   Secret ve Ingress de gider); silme başarısız olursa bir sonraki turda tekrar dener.

Emekli (`retired`) bir deploy'a rollback yapılamaz (`409`); aynı commit tekrar push'lanırsa
yeniden build edilir. Branch GitHub'da silinince ya da PR kapanınca o branch'in preview alias'ı
hemen kalkar ve deploy'ları emekliye ayrılır; production branch'i bu yolla asla temizlenmez.

Çökme sonrası kurtarma: worker çalıştığı deploy için heartbeat yazar. `PAAS_WORKER_STALE_AFTER`
(2 dk) boyunca heartbeat gelmezse deploy yeniden kuyruğa alınır; ikinci denemede de sahipsiz
kalırsa `failed` olur. Ayrıntılar ve tüm arıza durumları: [docs/FAILURE-SCENARIOS.md](docs/FAILURE-SCENARIOS.md).

| Değişken | Açıklama |
|---|---|
| `PAAS_KEEP_PRODUCTION` | rollback için tutulan production deploy sayısı (varsayılan `5`) |
| `PAAS_PREVIEW_TTL` | alias'sız preview'in emekliye ayrılma süresi (varsayılan `72h`) |
| `PAAS_PREVIEW_ALIAS_TTL` | hiç güncellenmeyen preview alias'ının kaldırılma süresi (varsayılan `0` = asla) |
| `PAAS_GC_INTERVAL` | temizlik turu aralığı (varsayılan `10m`) |
| `PAAS_WORKER_STALE_AFTER` | heartbeat'siz kalan deploy'un sahipsiz sayılma süresi (varsayılan `2m`) |

### Build nasıl çalışır

1. `git fetch --depth=1 <repo> <sha>` — branch değil SHA çekilir, kuyrukta beklerken gelen push build'i değiştiremez.
2. Algılama: repoda `Dockerfile` varsa o kullanılır; yoksa `package.json` → Node, `go.mod` → Go,
   `index.html` / `public/index.html` → nginx ile statik site. Örnekler: [examples/](examples/)
3. Üretilen Dockerfile build logunda aynen görünür.
4. İmaj `PAAS_REGISTRY/<app>:<sha>` olarak push edilir; deployment'a digest'li referans yazılır.

**Platform sözleşmesi:** uygulama `$PORT` (8080) üzerinden dinler.

| Değişken | Açıklama |
|---|---|
| `PAAS_BUILDER` | `dryrun` · `docker` (yerel, Docker Desktop) · `buildkit` (kümede, `buildctl`) |
| `PAAS_REGISTRY` | ör. `localhost:5000`, `ghcr.io/nisagwn`, ECR adresi |
| `PAAS_REGISTRY_INSECURE` | düz HTTP registry (yalnızca yerel) |
| `PAAS_BUILDKIT_ADDR` | `buildkitd` adresi, ör. `tcp://buildkitd:1234` |
| `PAAS_BUILD_PLATFORM` | boş = builder'ın kendi platformu; küme `linux/arm64` |
| `PAAS_BUILD_CACHE` | `true` → `<app>:buildcache` registry cache |
| `PAAS_GIT_BASE_URL` | `owner/repo` önüne eklenir (varsayılan `https://github.com`) |
| `PAAS_GITHUB_TOKEN` | private repolar için (Contents: read) |

### Web arayüzü ve canlı loglar (Faz 5)

Arayüz `/` adresinde: API token'ı ile bir kez giriş yapılır. Sayfalar:

- **Uygulamalar:** liste, her birinin son deploy durumu, yeni uygulama formu
- **Uygulama:** alias'lar, deploy geçmişi, tek tıkla rollback, ortam değişkeni anahtarları (değerler asla gösterilmez)
- **Deployment** (`/deployments/{id}`): canlı build logu, bitince durum rozeti güncellenir; Kubernetes'te pod logları

Güvenlik: oturum, API token'ından türetilen anahtarla HMAC imzalı, sunucuda durum tutmayan bir
çerezdir (`HttpOnly`, `SameSite=Strict`, HTTPS'te `Secure`); token değişince tüm oturumlar düşer.
Değişiklik yapan her form oturuma bağlı bir CSRF token'ı ve aynı-origin kontrolünden geçer.
Sayfalar sıkı bir CSP (istek başına nonce) ile sunulur.

Canlı log: yeni log satırı ve durum değişikliği Postgres'te `NOTIFY` üretir; SSE akışı önce
geçmişi, sonra canlı satırları gönderir, deployment bitince son `status` olayıyla kapanır.
`Last-Event-ID` ile kopan bağlantı kaldığı yerden devam eder.

### GitHub entegrasyonu (Faz 5)

`PAAS_GITHUB_TOKEN` tanımlıysa worker her deployment'ı GitHub'a bildirir:

| An | Commit status (`paas/deploy`) | Bağlantı |
|---|---|---|
| Worker deployment'ı aldı | `pending` — "Build başladı" | deployment sayfası |
| Deploy hazır | `success` — "Deploy hazır: `<sha7>-<app>.<domain>`" | deployment URL'si |
| Build/deploy hatası | `failure` — hata mesajı (140 karaktere kısaltılır) | deployment sayfası |

Deployment bittiğinde branch'in açık PR'ları (`GET /repos/{o}/{r}/pulls?head=<owner>:<branch>`) bulunur
ve her birinde uygulama başına **tek bir yorum** tutulur: gizli `<!-- paas:preview:<app> -->` işaretiyle
mevcut yorum bulunur ve güncellenir (`PATCH`), yoksa oluşturulur. Başarıda yorum preview, deployment
(production branch'te production) URL'lerini, commit'i ve durumu tablo olarak gösterir; hatada
"❌ Deploy başarısız" başlığıyla hata metnini ve log bağlantısını içerir.

- Deployment sayfası: `PAAS_PUBLIC_URL/deployments/<id>` (web arayüzü; loglar orada canlı izlenir).
  API karşılığı `GET /api/deployments/<id>/logs` (bearer token ister).
- Bildirimler deployment'ı asla düşürmez: her çağrının kendi zaman aşımı vardır (15 sn), hata `slog`'a
  ve deployment loguna `WARNING: notification failed: …` satırı olarak yazılır.
- Geç biten eski bir commit, PR'ın head'i ilerlemişse yorumu ezmez; yorumu yeni commit'in deployment'ı yazar.
- İstemci yalnızca `net/http` kullanır; hata yanıtları durum kodu ve GitHub mesajıyla döner, kalan istek
  hakkı 100'ün altına inince `X-RateLimit-*` başlıkları loglanır, rate limit hatasında bekleme süresi
  hata mesajında yer alır.
- **Fork kısıtı:** PR araması aynı repodaki branch'leri bulur (`head=<repo sahibi>:<branch>`). Fork'tan
  açılan PR'lar bulunmaz; fork'a yapılan push bu reponun webhook'unu tetiklemediği için zaten deploy edilmezler.

**Token izinleri:** fine-grained PAT (yalnızca ilgili repolar) — *Contents: Read*, *Commit statuses:
Read and write*, *Pull requests: Read and write* (PR yorumları issue comments API'si ile yazılır; GitHub
bunu PR'larda *Pull requests: write* izniyle kabul eder). Klasik token için `repo` kapsamı (yalnızca
public repolar için `public_repo`).

| Değişken | Açıklama |
|---|---|
| `PAAS_GITHUB_TOKEN` | hem private repo klonlama hem bildirimler için; boşsa bildirim yok |
| `PAAS_GITHUB_STATUS` | `false` → token olsa da status/yorum gönderilmez (varsayılan açık) |
| `PAAS_GITHUB_STATUS_CONTEXT` | commit status adı (varsayılan `paas/deploy`) |
| `PAAS_GITHUB_API_URL` | varsayılan `https://api.github.com`; GitHub Enterprise: `https://<host>/api/v3` |
| `PAAS_PUBLIC_URL` | kontrol düzleminin dış adresi, bağlantılar buraya gider (varsayılan `https://<PAAS_DOMAIN>`) |

## Yerel geliştirme

Gerekenler: Go 1.24+, Docker, `openssl`.

```bash
cp .env.example .env
make db          # Postgres'i Docker'da başlatır
make test        # tüm testler (veritabanı entegrasyon testleri dahil)
make test-build  # examples/ altındaki uygulamaları gerçekten build edip çalıştırır
make run         # API + worker + yerel registry  →  http://localhost:8080
```

Gerçek build için `.env` içinde `PAAS_BUILDER=docker` yap. `buildkit` modunu yerelde
denemek için `make buildkitd` (host'ta `buildctl` gerekir).

Başka bir terminalde:

```bash
source .env
H="Authorization: Bearer $PAAS_API_TOKEN"

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

# 6. Ortam değişkenleri (null siler; GET yalnızca anahtarları döner)
curl -s -H "$H" -X PUT -d '{"DATABASE_URL":"postgres://…","DEBUG":null}' localhost:8080/api/apps/blog/env
curl -s -H "$H" localhost:8080/api/apps/blog/env
```

### Gerçek GitHub'a bağlamak

Repo → Settings → Webhooks → Add webhook:
- **Payload URL:** `https://<sunucun>/webhooks/github` (yerelde test için ngrok veya Cloudflare Tunnel)
- **Content type:** `application/json`
- **Secret:** `PAAS_GITHUB_WEBHOOK_SECRET` ile aynı
- **Events:** "Let me select individual events" → **Pushes** ve **Pull requests**
  (PR kapanınca preview temizliği için)

## API

Tüm `/api/*` uçları `Authorization: Bearer <PAAS_API_TOKEN>` ister.

| Yöntem | Yol | Açıklama |
|---|---|---|
| GET | `/healthz` | Sağlık kontrolü (veritabanına ping atar) |
| POST | `/webhooks/github` | GitHub webhook alıcısı (imza ile korunur) |
| POST | `/api/apps` | `{"name","repo","production_branch"?}` |
| GET | `/api/apps` | Uygulamalar |
| GET | `/api/apps/{name}` | Uygulama + alias'lar |
| GET | `/api/apps/{name}/deployments?limit=` | Deployment geçmişi |
| POST | `/api/apps/{name}/rollback` | `{"deployment_id"}` — production alias'ını taşır |
| GET | `/api/apps/{name}/env` | `{"keys":[…]}` — değerler API'den asla okunmaz |
| PUT | `/api/apps/{name}/env` | `{"KEY":"değer","ESKI":null}` — birleştirir, `null` siler. `PORT` ve `PAAS_*` platforma ait |
| GET | `/api/deployments/{id}` | Tek deployment |
| GET | `/api/deployments/{id}/logs?after=` | Log satırları |
| GET | `/api/deployments/{id}/logs/stream` | Canlı log (SSE); tarayıcıda oturum çereziyle de çalışır |
| GET | `/api/apps/{name}/deployments/{id}/runtime-logs?follow=1&tail=200` | Pod logları (yalnızca `kubernetes` deployer) |

## Proje yapısı

```
cmd/paas/         giriş noktası: API + worker tek süreçte, graceful shutdown
internal/config/      ortam değişkenleri
internal/store/       PostgreSQL erişimi, migration'lar, kuyruk
internal/webhook/     GitHub imza doğrulama ve push ayrıştırma
internal/github/      GitHub REST istemcisi: commit status, PR arama, işaretli PR yorumu
internal/api/         HTTP uçları
internal/worker/      kuyruk tüketicisi, Builder/Deployer arayüzleri, dry-run pipeline
internal/build/       clone, dil algılama, Dockerfile üretimi, BuildKit/docker motorları
internal/deploy/      Kubernetes deployer: namespace, kota, Secret, Deployment, Service, Ingress, rollout bekleme
internal/routing/     alias'ları veritabanından ingress katmanına senkronlar, uzlaştırma döngüsü
internal/auth/        web oturumu (imzalı çerez) ve CSRF
internal/web/         web arayüzü (html/template + htmx)
internal/github/      GitHub commit status ve PR yorumları
internal/cleanup/     saklama politikası, emekliye ayırma, küme nesnelerinin silinmesi
loadtest/             k6 yük testleri
examples/             otomatik algılanan örnek uygulamalar (Node, Go, statik)
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
- **stdlib + lib/pq, tek bilinçli istisna client-go:** Bağımlılık sayısı bilinçli olarak
  düşük. Kubernetes API'si için `k8s.io/client-go` (typed clientset) kullanılır: kimlik
  doğrulama, kubeconfig/in-cluster yapılandırma ve API tipleri elle yazılmaya değmez.
  Yalnızca `internal/deploy` ona bağımlı.
- **Deploy idempotent:** Her nesne "varsa güncelle, yoksa oluştur"; worker deploy ortasında
  çökerse aynı deployment tekrar çalıştırılabilir. Değişmez `Secret`'ın verisi farklıysa silinip
  yeniden oluşturulur ve pod şablonundaki `paas/env-hash` rollout'u tetikler.
- **Polling, watch değil:** Rollout her 2 sn'de bir okunur; bağlantı kopmalarına dayanıklı,
  fake clientset ile test edilmesi kolay. Kalıcı hatalarda (`ImagePullBackOff`,
  `CrashLoopBackOff`, `CreateContainerConfigError`, kota aşımı) zaman aşımı beklenmez.
- **TCP readiness probe:** `/` rotası olmayan bir uygulama da hazır sayılır.
- **Ortam değişkenleri Postgres'te düz metin:** API değerleri asla geri döndürmez; Kubernetes
  tarafında Secret olarak durur. Veritabanında şifreleme sonraki bir adım.
