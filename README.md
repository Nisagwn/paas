# paas

Vercel tarzı, Git tabanlı PaaS: repoyu bağla, `git push` yap; her commit kendi HTTPS adresinde çalışan,
değişmez bir deploy olur. Go ile yazıldı, Kubernetes (k3s) üzerinde çalışır, arayüzü Türkçedir.

## Neler yapar

- **Her dil:** repodaki `Dockerfile` kullanılır; yoksa Node (Next.js, Vite, Nuxt, SvelteKit, Astro, Remix, NestJS…),
  Go, Python (Django, FastAPI, Flask), Ruby (Rails, Rack), Java (Maven, Gradle) ve statik siteler otomatik algılanır,
  Dockerfile platform tarafından üretilir. Monorepo kök dizini ve build / start komutları proje başına ayarlanır.
- **Branch başına preview:** her branch `<branch>-<app>.<domain>` adresini alır; PR'a "Preview hazır" yorumu ve
  commit status'u yazılır, branch silinince preview de kalkar.
- **Değişmez deploy'lar, anında rollback:** her commit `<sha7>-<app>.<domain>` adresinde kalıcıdır; production yalnızca
  bir takma addır, rollback build'siz ve milisaniyeler içinde olur. Preview'i yeniden build etmeden production'a taşıma,
  yeniden deploy, iptal ve deploy hook'ları da var.
- **Web dışı süreçler:** `paas.yaml` ile aynı imajdan worker'lar, cron işleri ve kalıcı WebSocket sunucuları; rollback
  onları da eski sürüme taşır.
- **Yönetilen PostgreSQL / Redis:** tek tıkla eklenir, `DATABASE_URL` / `REDIS_URL` deploy'lara kendiliğinden girer;
  her preview branch'i production veritabanının (istenirse anonimleştirilmiş) kendi kopyasıyla çalışır. Günlük yedek ve
  geri yükleme dahil.
- **Kademeli yayın (canary):** yeni production önce trafiğin bir payını alır, 5xx oranı ve p95 gecikmesi mevcut
  production ile karşılaştırılır; bozulursa kendiliğinden geri alınır.
- **Deploy koruması:** deploy ve preview adresleri yalnızca ekibe, ortak şifreyle ya da paylaşım bağlantısıyla açılır;
  CI için otomasyon anahtarı.
- **Preview yorumları ve bildirimler:** ekip deploy'un üzerine noktaya sabitlenmiş yorumlar bırakır (PR'a da yazılır);
  deploy, production değişikliği ve geri alma olayları e-posta, Slack, Discord ya da imzalı webhook ile gelir.
- **Tarayıcıdan terminal:** çalışan pod'da xterm.js ile kabuk; kubeconfig gerekmez, her oturum denetim kaydına yazılır.
- **Ekipler ve planlar:** GitHub ile giriş, roller (owner / member / viewer), kişisel API token'ları, `paas` CLI;
  Hobby / Pro / Sınırsız planları, hız sınırları ve askıya alma ile herkese açık kayıt.

Bunların yanında özel alan adları (otomatik Let's Encrypt), sıfıra ölçekleme, istek analitiği (p50 / p95, 5xx), şifreli
ortam değişkenleri, otomatik temizlik ve çökme sonrası kurtarma vardır. Her özelliğin ayrıntısı:
**[docs/KULLANIM.md](docs/KULLANIM.md)**.

## Mimari

![Sistem mimarisi](docs/images/mimari-genel.svg)

- **Kontrol düzlemi** (`cmd/paas`) tek bir Go ikilisidir: HTTP API, web arayüzü, GitHub webhook alıcısı, build / deploy
  worker'ları ve arka plan döngüleri (alias senkronu, temizlik, ölçekleme, canary, eklentiler, bildirimler) aynı süreçte.
- **PostgreSQL** hem durumun tek doğru kaynağı hem iş kuyruğudur (`FOR UPDATE SKIP LOCKED`); canlı loglar
  `LISTEN/NOTIFY` ile akar.
- **Akış:** `git push` → GitHub webhook'u (HMAC imzalı) → deploy kuyruğa girer → worker commit'i SHA ile çeker → BuildKit
  imajı build edip registry'ye (ECR ya da yerel) gönderir → Kubernetes'te uygulama namespace'ine `Deployment` + `Service` +
  `Ingress` yazılır → pod hazır olunca production / preview alias'ları taşınır → GitHub'a commit status ve PR yorumu gider.
- **Traefik** tüm adresleri yönlendirir (wildcard ya da host başına TLS, cert-manager); canary için ağırlıklı servis,
  koruma için ForwardAuth kullanılır.
- Kullanıcı uygulamaları ayrı namespace'lerde, kota, `NetworkPolicy` ve root olmayan kullanıcıyla çalışır.

Ayrıntılı mimari: [docs/MIMARI.md](docs/MIMARI.md)

## Hızlı başlangıç (yerel)

### Gerekenler

| Araç | Not |
|---|---|
| Go **1.24+** | `go.mod`: `go 1.24.0` |
| Docker Desktop (ya da Docker Engine + Compose v2) | Postgres ve registry için; gerçek build'lerde `docker buildx` |
| Git | Gerçek build'ler commit'i `git fetch` ile çeker |
| [k3d](https://k3d.io) 5.x ve `kubectl` | Yalnızca gerçek deploy modu (b) için |

Windows'ta komutlar PowerShell'de çalışır; `scripts/*.sh` için **Git Bash** gerekir (Git for Windows ile gelir,
`openssl` dahil). `make` zorunlu değildir; aşağıdaki komutlar `make` olmadan yazıldı.

### 1. Repo, veritabanı ve `.env`

```bash
git clone https://github.com/Nisagwn/paas.git
cd paas
docker compose up -d postgres registry     # Postgres :5432, imaj registry'si :5000
cp .env.example .env                        # PowerShell: Copy-Item .env.example .env
```

> macOS'ta 5000 portunu AirPlay Alıcısı kullanır; ya onu kapatın ya da yalnızca `postgres`'i başlatıp mod (b)'yi kullanın.

`.env.example` yerel geliştirme için hazırdır. Yerelde önemli olan anahtarlar:

| Anahtar | Varsayılan | Anlamı |
|---|---|---|
| `PAAS_DATABASE_URL` | `postgres://paas:paas@localhost:5432/paas?sslmode=disable` | Compose'daki Postgres |
| `PAAS_API_TOKEN` | `dev-token-change-me` | Admin token'ı: API'de `Authorization: Bearer …`, GitHub girişi yokken arayüzde token girişi |
| `PAAS_SESSION_KEY` | boş | Oturum çerezlerinin imza anahtarı (en az 32 karakter); boşsa API token'ından türetilir, GitHub girişi açık ve API token'ı boşsa zorunlu |
| `PAAS_GITHUB_WEBHOOK_SECRET` | `dev-webhook-secret` | Webhook HMAC imzasının sırrı; GitHub'daki (App ya da repo webhook'u) değerle aynı olmalı |
| `PAAS_BUILDER` | `dryrun` | `dryrun` (taklit) · `docker` (yerel Docker) · `buildkit` (kümede `buildctl`) |
| `PAAS_DEPLOYER` | `dryrun` | `dryrun` (taklit) · `kubernetes` |
| `PAAS_PUBLIC_URL` | boş → `https://<PAAS_DOMAIN>` | Arayüzün dış adresi; yerelde **`http://localhost:8080`** yazın (bağlantılar ve GitHub girişi bunu kullanır) |
| `PAAS_ENV_KEY` | boş | Ortam değişkenlerini şifreleyen anahtar (`go run ./cmd/paas-envkey` üretir); boşsa düz metin, yalnızca geliştirme |

Sunucu `.env`'yi çalışma dizininden kendisi okur (ortamda tanımlı değişkenler önceliklidir).

### 2a. Dry-run modu (küme yok)

Build ve deploy taklit edilir: arayüz, API, kuyruk, alias'lar ve rollback Kubernetes kümesi olmadan denenir. `.env`'de `PAAS_BUILDER=dryrun`, `PAAS_DEPLOYER=dryrun` (varsayılan) olsun:

```bash
go run ./cmd/paas          # http://localhost:8080
```

http://localhost:8080 adresini açın, giriş sayfasına `PAAS_API_TOKEN` değerini (`dev-token-change-me`) yazın.

Uygulama oluşturup bir push'u taklit etmek (ikinci bir terminalde):

```bash
# Linux / macOS / Git Bash
H="Authorization: Bearer dev-token-change-me"
curl -s -H "$H" -d '{"name":"blog","repo":"nisagwn/blog"}' localhost:8080/api/apps
PAAS_GITHUB_WEBHOOK_SECRET=dev-webhook-secret scripts/push.sh nisagwn/blog main    # imzalı push webhook'u
curl -s -H "$H" localhost:8080/api/apps/blog/deployments                           # birkaç saniye sonra "ready"
curl -s -H "$H" localhost:8080/api/deployments/1/logs
```

```powershell
# Windows PowerShell
$H = @{ Authorization = "Bearer dev-token-change-me" }
Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/apps -Headers $H -ContentType application/json `
  -Body '{"name":"blog","repo":"nisagwn/blog"}'
# İmzalı push webhook'u (scripts/push.sh'in karşılığı)
$body = '{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567","repository":{"full_name":"nisagwn/blog"},"head_commit":{"message":"deneme"}}'
$hmac = New-Object System.Security.Cryptography.HMACSHA256
$hmac.Key = [Text.Encoding]::UTF8.GetBytes("dev-webhook-secret")
$sig = "sha256=" + (($hmac.ComputeHash([Text.Encoding]::UTF8.GetBytes($body)) | ForEach-Object { $_.ToString("x2") }) -join "")
Invoke-RestMethod -Method Post -Uri http://localhost:8080/webhooks/github -ContentType application/json `
  -Headers @{ "X-GitHub-Event" = "push"; "X-Hub-Signature-256" = $sig } -Body $body
Invoke-RestMethod -Uri http://localhost:8080/api/apps/blog/deployments -Headers $H
```

Deploy listesinde `"status":"ready"` ve production alias'ı (`blog.localtest.me`) görünür; arayüzde canlı log, rollback ve
diğer ekranlar çalışır.

### 2b. Gerçek build ve deploy (k3d)

Uygulamalar gerçekten build edilir ve yerel bir Kubernetes kümesinde `http://<ad>.localtest.me` adreslerinde çalışır
(`*.localtest.me` her zaman `127.0.0.1`'e çözülür). Host'ta 80 portu boş olmalıdır.

```bash
docker compose up -d postgres    # bu modda compose registry'si gerekmez
scripts/k3d-up.sh                # k3s + Traefik + registry (localhost:5111); Windows'ta Git Bash'te
kubectl get nodes                # k3d-paas-server-0 Ready
```

`.env`'de:

```ini
PAAS_BUILDER=docker
PAAS_REGISTRY=localhost:5111
PAAS_DEPLOYER=kubernetes
PAAS_INGRESS_TLS=false
PAAS_DOMAIN=localtest.me
PAAS_PUBLIC_URL=http://localhost:8080
# Repolar GitHub'dan çekilir. Kendi makinenizdeki repolar için bir kök dizin verin:
# uygulamanın "owner/repo" değeri bu dizine eklenir (Windows: file:///C:/Users/<ad>/repos).
PAAS_GIT_BASE_URL=file:///home/<ad>/repos
```

Ardından `go run ./cmd/paas`. Örnek uygulamalardan birini yerel bir repo olarak deploy etmek:

```bash
mkdir -p ~/repos/demo && cp -r examples/node-hello ~/repos/demo/hello
cd ~/repos/demo/hello && git init -q && git add . && git commit -qm ilk && cd -
H="Authorization: Bearer dev-token-change-me"
curl -s -H "$H" -d '{"name":"hello","repo":"demo/hello"}' localhost:8080/api/apps
PAAS_GITHUB_WEBHOOK_SECRET=dev-webhook-secret \
  scripts/push.sh demo/hello main "$(git -C ~/repos/demo/hello rev-parse HEAD)"
```

Build logu arayüzde akar; hazır olunca `http://hello.localtest.me` (production), `http://main-hello.localtest.me`
(branch) ve `http://<sha7>-hello.localtest.me` (commit) açılır. `examples/` altında Next.js, Vite, Go, Python, Ruby,
Java, statik site, WebSocket ve worker örnekleri var ([examples/README.md](examples/README.md)).

İsteğe bağlı: `scripts/k3d-up.sh`'in sonunda yazdırdığı `PAAS_ACTIVATOR_IP` ve
`PAAS_ACTIVATOR_UPSTREAM=http://127.0.0.1:80` sıfıra ölçeklemeyi, `PAAS_FORWARD_AUTH_URL` deploy korumasını açar
([KULLANIM.md](docs/KULLANIM.md#sıfıra-ölçekleme-faz-11)). Kümeyi kaldırmak: `scripts/k3d-up.sh down`.

### GitHub ile giriş ve gerçek repolar

- **Giriş:** GitHub girişi yapılandırılmamışsa (geliştirme modu) arayüz `PAAS_API_TOKEN` ile açılır.
  `PAAS_GITHUB_OAUTH_CLIENT_ID` / `_SECRET` doldurulunca "GitHub ile giriş yap" çıkar ve token formu kapanır; GitHub'daki
  callback adresi **`<PAAS_PUBLIC_URL>/auth/github/callback`** olmalıdır. Kendinizi `PAAS_ADMIN_GITHUB_LOGINS`'e yazın
  (aksi halde kimse giremez), herkese açık kayıt için `PAAS_OPEN_SIGNUP=true`.
- **Push'lar:** GitHub'ın webhook'ları makinenize ulaşabilmeli: `cloudflared tunnel --url http://localhost:8080` ya da
  `ngrok http 8080` ile açılan adresi `PAAS_PUBLIC_URL` yapın. En kolay yol bir GitHub App'tir (aşağıda); App'siz,
  repo başına webhook yolu [KULLANIM.md](docs/KULLANIM.md#gerçek-githuba-bağlamak)'de.

### Testler

```bash
# Linux / macOS / Git Bash
go vet ./...
PAAS_TEST_DATABASE_URL="postgres://paas:paas@localhost:5432/paas_test?sslmode=disable" go test -p 1 ./...
```

```powershell
# Windows PowerShell
$env:PAAS_TEST_DATABASE_URL = "postgres://paas:paas@localhost:5432/paas_test?sslmode=disable"
go test -p 1 ./...
```

- `paas_test` veritabanını Compose ilk açılışta oluşturur (`scripts/init-test-db.sql`). Postgres volume'ü daha eskiyse:
  `docker exec paas-postgres-1 psql -U paas -d postgres -c "CREATE DATABASE paas_test"`.
- Testler veritabanını sıfırlar: paketler aynı veritabanını paylaştığı için `-p 1` şarttır ve **aynı anda çalışan her
  test koşusu kendi veritabanını** kullanmalıdır (ör. `paas_test_2`). `PAAS_TEST_DATABASE_URL` yoksa veritabanı testleri atlanır.
- `make test` aynı komutu `-race` ile çalıştırır (cgo ve bir C derleyicisi ister). `make test-build`
  (`PAAS_TEST_DOCKER_BUILD=1`) `examples/` altındaki uygulamaları Docker ile gerçekten build edip çalıştırır.

### CLI

```bash
go build -o paas ./cmd/paas-cli        # Windows: go build -o paas.exe ./cmd/paas-cli
paas login --url http://localhost:8080  # /tokens sayfasında oluşturulan kişisel token'ı sorar
paas ls
paas logs 1 -f
```

Tüm komutlar: [KULLANIM.md → CLI](docs/KULLANIM.md#cli-faz-18).

## GitHub App kurulumu

Tek bir GitHub App giriş, repo erişimi, webhook'lar, commit status'ları ve PR yorumlarının hepsini karşılar; yeni bir
repoyu bağlamak GitHub'da ayar gerektirmez (arayüzde *Import from GitHub*).

1. github.com → *Settings* → *Developer settings* → *GitHub Apps* → *New GitHub App* (organizasyon adına: organizasyon
   *Settings* → *Developer settings*).
2. Adresler (`PAAS_PUBLIC_URL` ör. `https://paas.example.com`):

   | Alan | Değer |
   |---|---|
   | *Homepage URL* | `<PAAS_PUBLIC_URL>` |
   | *Callback URL* | `<PAAS_PUBLIC_URL>/auth/github/callback` |
   | *Request user authorization (OAuth) during installation* | **kapalı** (açıkken Setup URL devre dışı kalır) |
   | *Setup URL* | `<PAAS_PUBLIC_URL>/github/setup`, *Redirect on update* **açık** |
   | *Webhook URL* | `<PAAS_PUBLIC_URL>/webhooks/github`, *Active* açık |
   | *Webhook secret* | `PAAS_GITHUB_WEBHOOK_SECRET` ile aynı değer |

3. *Repository permissions:* **Contents** Read-only, **Commit statuses** Read and write, **Pull requests** Read and
   write, **Metadata** Read-only (zorunlu). *Organization permissions → Members* Read-only yalnızca
   `PAAS_ALLOWED_GITHUB_ORG` kullanılıyorsa.
4. *Subscribe to events:* **Push** ve **Pull request** (`installation` olayları her zaman gelir).
5. *Where can this GitHub App be installed?* **Any account**: herkese açık kayıt (`PAAS_OPEN_SIGNUP=true`) için şarttır;
   yalnızca kendi hesabınızın repolarını deploy edecekseniz *Only on this account*.
6. *Create GitHub App* → *General* sayfasından `.env`'ye:

   | GitHub | `.env` |
   |---|---|
   | *App ID* | `PAAS_GITHUB_APP_ID` |
   | URL'deki ad (`github.com/apps/<slug>`) | `PAAS_GITHUB_APP_SLUG` |
   | *Client ID* | `PAAS_GITHUB_OAUTH_CLIENT_ID` |
   | *Generate a new client secret* | `PAAS_GITHUB_OAUTH_CLIENT_SECRET` |
   | *Generate a private key* (inen `.pem`) | `PAAS_GITHUB_APP_PRIVATE_KEY_FILE=./github-app.pem` (ya da PEM içeriği `PAAS_GITHUB_APP_PRIVATE_KEY`'e) |

   Ayrıca `PAAS_ADMIN_GITHUB_LOGINS=<github-kullanıcı-adınız>`. `.pem` dosyasını repoya koymayın. App değişkenlerinden
   yalnızca bir kısmını doldurmak açılışta hata verir.
7. Sunucuyu yeniden başlatın, GitHub ile girin, *Import from GitHub* → *Install GitHub App* → repoları seçin → *Import*.

Kurulumun ekibe bağlanmasının güvenlik ayrıntıları ve App'siz (PAT + repo webhook'u) yol:
[KULLANIM.md → GitHub App](docs/KULLANIM.md#github-app-faz-15).

## Canlıya alma (AWS)

`infra/terraform` boş bir AWS hesabından çalışan bir platform kurar: VPC, tek bir Graviton EC2 (k3s, Traefik,
cert-manager, rootless BuildKit, Postgres), Elastic IP, ECR ve IAM. Aylık maliyet ~37–40 $ (t4g.medium). Tüm ayrıntılar,
tasarım kararları ve gün-2 işlemleri: **[infra/README.md](infra/README.md)**.

Gerekenler: Terraform ≥ 1.6, AWS CLI v2 (`aws configure`), Docker buildx, `kubectl`, bir SSH anahtarı ve bir alan adı
(Route 53 hosted zone **ya da** ücretsiz bir DuckDNS adı).

```bash
# 1. Altyapı
cd infra/terraform
cp terraform.tfvars.example terraform.tfvars   # region, domain, dns_provider, letsencrypt_email, ssh_public_key, admin_cidrs
terraform init && terraform plan -out tfplan && terraform apply tfplan

# 2. DNS — DuckDNS kullanılıyorsa adı Elastic IP'ye yönlendirin (Route 53 kayıtlarını Terraform yazar)
curl "https://www.duckdns.org/update?domains=<ad>&token=$DUCKDNS_TOKEN&ip=$(terraform output -raw public_ip)"

# 3. kubectl erişimi (bootstrap 5–10 dk sürer: $(terraform output -raw ssh) sudo tail -f /var/log/paas-bootstrap.log)
eval "$(terraform output -raw kubeconfig_command)"
export KUBECONFIG=$PWD/kubeconfig-paas.yaml
cd ../..

# 4. Kontrol düzlemi imajı (node arm64)
REPO=$(terraform -chdir=infra/terraform output -raw control_plane_repository_url)
REGISTRY=$(terraform -chdir=infra/terraform output -raw ecr_registry)
aws ecr get-login-password --region eu-central-1 | docker login --username AWS --password-stdin "$REGISTRY"
docker buildx build --platform linux/arm64 -t "$REPO:latest" --push .
kubectl -n paas rollout restart deployment/paas
```

**5. Secret'lar.** Veritabanı parolası, API token'ı, webhook sırrı ve env anahtarı node'da üretilir (Terraform state'ine
girmez). GitHub App değerlerini `paas` Secret'ına ekleyin, webhook sırrını App ayarlarına yazın:

```bash
kubectl -n paas get secret paas -o go-template='{{index .data "github-webhook-secret" | base64decode}}'   # → App'in Webhook secret'ı
kubectl -n paas patch secret paas -p '{"stringData":{
  "github-app-id":"123456","github-app-slug":"paas-ornek",
  "github-oauth-client-id":"Iv1....","github-oauth-client-secret":"...",
  "admin-github-logins":"<github-kullanıcı-adınız>","open-signup":"true"}}'
kubectl -n paas patch secret paas -p "{\"data\":{\"github-app-private-key\":\"$(base64 -w0 github-app.pem)\"}}"   # macOS: base64 -i github-app.pem
kubectl -n paas rollout restart deployment/paas
```

**6. Manifest değişiklikleri** (`infra/k8s/*.yaml`) node yeniden yaratılmadan uygulanır:

```bash
cd infra/terraform && eval "$(terraform output -raw k8s_env)"
VARS='${DOMAIN} ${AWS_REGION} ${HOSTED_ZONE_ID} ${LETSENCRYPT_EMAIL} ${ACME_SERVER} ${PAAS_REGISTRY} ${CONTROL_PLANE_IMAGE} ${PAAS_DEPLOYER}'
for f in ../k8s/*.yaml; do envsubst "$VARS" <"$f"; echo '---'; done | kubectl apply -f -
```

**7. GitHub App adresleri.** App'in *Homepage*, *Callback*, *Setup* ve *Webhook* URL'lerini `https://<domain>` ile
güncelleyin (her yeni kurulumda alan adı ya da IP değiştiyse).

**8. Kaldırma.** Krediyi korumak için kullanılmadığında altyapıyı silin; Postgres verisi node'un diskindedir, önce yedek alın:

```bash
kubectl -n paas exec postgres-0 -- pg_dump -U paas paas > paas.sql
for r in $(aws ecr describe-repositories --region eu-central-1 \
    --query "repositories[?starts_with(repositoryName, 'paas/')].repositoryName" --output text); do
  aws ecr delete-repository --region eu-central-1 --force --repository-name "$r"
done
terraform -chdir=infra/terraform destroy
```

İlk kurulumda `letsencrypt_environment = "staging"` önerilir; her şey çalışınca `"production"`.

### Domain

| Adres | Ne gösterir |
|---|---|
| `https://<domain>` | Kontrol paneli, API ve `/webhooks/github` |
| `https://<app>.<domain>` | Production (production branch'in son deploy'u ya da rollback hedefi) |
| `https://<branch>-<app>.<domain>` | Branch'in son başarılı deploy'u (preview) |
| `https://<sha7>-<app>.<domain>` | Tek bir commit'in deploy'u; asla değişmez |

- Kullanıcılar projelerine **özel alan adı** ekler: alt alan adları için `CNAME` → `<app>.<domain>`, kök alan adları için
  `TXT _paas-challenge.<ad>` doğrulaması + `A` kaydı. Doğrulanınca alan adı production'ı izler ve kendi Let's Encrypt
  sertifikasını (HTTP-01) alır ([KULLANIM.md](docs/KULLANIM.md#özel-alan-adları-faz-12)).
- DuckDNS denemeler için uygundur ama wildcard sertifika alınamaz (host başına HTTP-01, haftalık Let's Encrypt sınırı).
- **Önerilen üretim kurulumu:** gerçek bir alan adı, DNS'i Cloudflare'de, `<domain>` ve `*.<domain>` kayıtları sunucunun
  IP'sine (wildcard DNS) ve DNS-01 ile tek wildcard sertifika. Hazır kurulum DNS-01'i Route 53 ile yapar
  (`infra/k8s/40-tls.yaml`); Cloudflare için ClusterIssuer'a cert-manager'ın Cloudflare çözücüsü ve bir API token'ı
  verilir, iki DNS kaydı Cloudflare'de Elastic IP'ye yazılır (`terraform output -raw public_ip`). İsteğe bağlı olarak kontrol panelini ve kullanıcı uygulamalarını
  ayrı alan adlarında sunmak (Vercel'deki `vercel.com` / `vercel.app` ayrımı gibi) çerezleri ve itibarı ayırır:
  uygulamalar `PAAS_DOMAIN`'de, panel `PAAS_PUBLIC_URL`'de; panelin host'u `infra/k8s/31-control-plane.yaml`'daki
  `IngressRoute`'ta ayarlanır.

## Proje yapısı

```
cmd/
  paas/             kontrol düzlemi: API, web arayüzü, webhook, worker'lar ve arka plan döngüleri
  paas-cli/         `paas` komut satırı aracı
  paas-dockerfile/  bir dizin için üretilecek Dockerfile'ı yazdırır
  paas-envkey/      env şifreleme anahtarı: üretme, durum, rotasyon
internal/
  api/  web/        JSON API ve Türkçe web arayüzü (html/template + htmx)
  store/            PostgreSQL: migration'lar, kuyruk, LISTEN/NOTIFY
  webhook/  github/ GitHub webhook'u, App, commit status ve PR yorumları
  worker/           kuyruk tüketicisi, heartbeat, çökme sonrası kurtarma
  build/            clone, dil / framework algılama, Dockerfile üretimi, BuildKit / docker
  deploy/  routing/ Kubernetes nesneleri, alias senkronu ve TLS
  process/          paas.yaml / Procfile: web, worker, cron
  rollout/          canary ve otomatik geri alma
  addons/           yönetilen Postgres / Redis, preview kopyaları, yedekler
  domains/  protect/ özel alan adları, deploy koruması (ForwardAuth)
  scale/  analytics/ sıfıra ölçekleme (aktivatör), istek metrikleri
  notify/  terminal/ wsock/   bildirimler, tarayıcı terminali, WebSocket (RFC 6455)
  auth/  plans/  ratelimit/  secret/   oturumlar, planlar, hız sınırları, AES-256-GCM
  cleanup/  appdelete/  cli/  config/  naming/  testdb/
examples/           otomatik algılanan örnek uygulamalar
infra/              Terraform (AWS) ve Kubernetes manifest'leri
scripts/            k3d kümesi, sahte push, ölçüm script'leri
loadtest/           k6 yük testleri
docs/               kullanım kılavuzu, mimari, rapor, ölçümler, arıza senaryoları
```

## Belgeler

| Belge | İçerik |
|---|---|
| [docs/KULLANIM.md](docs/KULLANIM.md) | Özellik kılavuzu: ayar değişkenleri, alan adları, preview'ler, rollback, süreçler, canary, veritabanları, koruma, yorumlar, bildirimler, terminal, planlar, silme, CLI, API tablosu |
| [docs/MIMARI.md](docs/MIMARI.md) | Ayrıntılı mimari |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Bağlam ve konteyner diyagramları, deploy / rollback akışları, durum makinesi, veri modeli |
| [docs/REPORT.md](docs/REPORT.md) | Teknik rapor: tasarım kararları, güvenlik, test, ölçümler |
| [docs/MEASUREMENTS.md](docs/MEASUREMENTS.md) | Kuyruk ölçeklenmesi, build süreleri, rollback gecikmesi, sıfıra ölçekleme |
| [docs/FAILURE-SCENARIOS.md](docs/FAILURE-SCENARIOS.md) | Arıza senaryoları, beklenen davranış ve testleri |
| [docs/DEMO.md](docs/DEMO.md) | Demo senaryosu |
| [infra/README.md](infra/README.md) | AWS kurulumu (Terraform, k3s) |
| [loadtest/README.md](loadtest/README.md) | k6 yük testleri |

## Yol haritası

| Faz | Konu |
|---|---|
| 0–1 | Hazırlık; kontrol düzlemi: API, imzalı GitHub webhook'u, Postgres kuyruğu |
| 2 | Build hattı: SHA ile clone, dil algılama, BuildKit, registry |
| 3 | Kubernetes'e deploy: namespace, kota, `Deployment` / `Service` / `Secret` |
| 4 | Yönlendirme, alias'lar, wildcard TLS, anında rollback |
| 5 | Canlı loglar, commit status ve PR yorumu, web arayüzü |
| 6 | Terraform + k3s ile AWS kurulumu |
| 7 | Saklama ve temizlik, çökme sonrası kurtarma, yük testleri |
| 8 | Belgeler ve ölçümler |
| 9 | Python, Ruby, Java build'leri |
| 10 | Ortam değişkenlerinin şifrelenmesi ve anahtar rotasyonu |
| 11 | Sıfıra ölçekleme |
| 12 | Özel alan adları |
| 13 | Kullanıcılar, ekipler, roller, kişisel token'lar |
| 14 | Daha hızlı ılık build |
| 15 | GitHub App ile import |
| 16 | Proje ayarları ve framework preset'leri |
| 17 | Ortamlar, promote, redeploy, iptal, deploy hook'ları |
| 18 | `paas` CLI |
| 19 | Analitik ve deploy sağlığı |
| 20 | Süreç tipleri: web, worker, cron |
| 21 | Kademeli yayın (canary) ve otomatik geri alma |
| 22 | Yönetilen veritabanları ve preview başına veritabanı kopyası |
| 23 | Herkese açılış: planlar, limitler, hız sınırları |
| 24 | Deploy koruması |
| 25 | Preview yorumları |
| 26 | Bildirimler |
| 27 | Tarayıcıdan terminal |
| 28 | Proje silme |
| Sonra | Faturalandırma, çok düğümlü küme, S3 uyumlu yedek hedefi, Postgres okuma kopyaları |

Her fazın kapsamı ve doğrulama durumu: [docs/PHASES.md](docs/PHASES.md).
