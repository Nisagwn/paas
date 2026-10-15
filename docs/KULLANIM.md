# paas kullanım kılavuzu

Platformun her özelliğinin ayrıntılı anlatımı: nasıl çalıştığı, ayar değişkenleri, API çağrıları ve CLI
komutları. Kurulum ve ilk deploy için [README](../README.md), bileşenler ve akışlar için
[MIMARI.md](MIMARI.md) ile [ARCHITECTURE.md](ARCHITECTURE.md).

Örneklerdeki `curl` komutları yerel kurulumu (`localhost:8080`) ve bir token'ı varsayar. Bazı örnekler
`$TOKEN`, bazıları `$PAAS_API_TOKEN` ya da `$H` kullanır; üçü de aynı token'dır:

```bash
export TOKEN=dev-token-change-me             # PAAS_API_TOKEN ya da /tokens sayfasında oluşturulan kişisel token
export PAAS_API_TOKEN=$TOKEN
H="Authorization: Bearer $TOKEN"
```

## İçindekiler

- [Build nasıl çalışır](#build-nasıl-çalışır)
- [Proje ayarları (Faz 16)](#proje-ayarları-faz-16)
- [Ortamlar ve deploy kontrolleri (Faz 17)](#ortamlar-ve-deploy-kontrolleri-faz-17)
- [Ortam değişkenlerinin şifrelenmesi (Faz 10)](#ortam-değişkenlerinin-şifrelenmesi-faz-10)
- [Deploy nasıl çalışır](#deploy-nasıl-çalışır)
- [Yönlendirme ve rollback (Faz 4)](#yönlendirme-ve-rollback-faz-4)
- [Yaşam döngüsü ve temizlik (Faz 7)](#yaşam-döngüsü-ve-temizlik-faz-7)
- [Özel alan adları (Faz 12)](#özel-alan-adları-faz-12)
- [Süreç tipleri (Faz 20)](#süreç-tipleri-faz-20)
- [Kademeli yayın (Faz 21)](#kademeli-yayın-faz-21)
- [Veritabanları (Faz 22)](#veritabanları-faz-22)
- [Sıfıra ölçekleme (Faz 11)](#sıfıra-ölçekleme-faz-11)
- [Analitik (Faz 19)](#analitik-faz-19)
- [Deploy koruması (Faz 24)](#deploy-koruması-faz-24)
- [Preview yorumları (Faz 25)](#preview-yorumları-faz-25)
- [Bildirimler (Faz 26)](#bildirimler-faz-26)
- [Tarayıcıdan terminal (Faz 27)](#tarayıcıdan-terminal-faz-27)
- [Kullanıcılar ve ekipler (Faz 13)](#kullanıcılar-ve-ekipler-faz-13)
- [GitHub entegrasyonu (Faz 5)](#github-entegrasyonu-faz-5)
- [GitHub App (Faz 15)](#github-app-faz-15)
- [Gerçek GitHub'a bağlamak](#gerçek-githuba-bağlamak)
- [Planlar ve limitler (Faz 23)](#planlar-ve-limitler-faz-23)
- [Proje silme (Faz 28)](#proje-silme-faz-28)
- [Web arayüzü ve canlı loglar (Faz 5)](#web-arayüzü-ve-canlı-loglar-faz-5)
- [CLI (Faz 18)](#cli-faz-18)
- [API](#api)
- [Tasarım kararları](#tasarım-kararları)
## Build nasıl çalışır

1. `git fetch --depth=1 <repo> <sha>` — branch değil SHA çekilir, kuyrukta beklerken gelen push build'i değiştiremez.
2. Algılama: repoda `Dockerfile` varsa o kullanılır. Yoksa sırayla:
   - `pom.xml` / `build.gradle(.kts)` → Java 21: Maven / Gradle (varsa `./mvnw` / `./gradlew`), üretilen en büyük
     jar `eclipse-temurin:21-jre` üzerinde `java -jar`; Spring Boot için `SERVER_PORT=8080`
   - `Gemfile` + `config.ru` → Ruby: `bin/rails` varsa Rails (`rails server`, assets precompile), yoksa Rack
     (`puma` ya da `rackup`); `ruby:3.3` ile build, `ruby:3.3-slim` ile çalışır, sürüm `Gemfile` / `.ruby-version`'dan
   - `requirements.txt` / `pyproject.toml` + `manage.py` → Django (`gunicorn <proje>.wsgi`)
   - `package.json` → Node: bağımlılıklardan framework preset'i seçilir (Next.js, Nuxt, SvelteKit, …; bkz.
     [Proje ayarları](#proje-ayarları-faz-16)), `go.mod` → Go
   - `requirements.txt` / `pyproject.toml` → Python: `uv.lock` → uv, `poetry.lock` → poetry, yoksa pip;
     FastAPI / Starlette → `uvicorn`, Flask → `gunicorn`, yoksa `python main.py` / `app.py`.
     Bağımlılıklarda olmayan sunucu paketi eklenir; sürüm `.python-version` / `runtime.txt`'den (varsayılan 3.12)
   - `Gemfile` + `Procfile` → Ruby
   - `index.html` / `public/index.html` → nginx ile statik site

   Java, Rails/Rack ve Django öne alınır, çünkü bu projelerde ön yüz araçları için sıkça `package.json` bulunur.
   `Procfile`'daki `web:` satırı Node, Python, Ruby ve Java'da başlatma komutunun yerine geçer (`sh -c` ile,
   `$PORT` genişler); Go ve statik imajlarda kabuk olmadığından yok sayılır. Örnekler: [examples/](../examples/)
3. Build logunda önce algılanan framework (`==> framework: Next.js`), ardından üretilen Dockerfile aynen görünür.
4. İmaj `PAAS_REGISTRY/<app>:<sha>-<deploy id>` olarak push edilir (her deploy'un kendi etiketi; Faz 30);
   deployment'a digest'li referans yazılır.

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


## Proje ayarları (Faz 16)

Vercel'deki "Build & Development Settings" karşılığı: her uygulamanın build'i algılanan varsayılanlardan
sapabilir. Boş bırakılan her alan "algılananı kullan" demektir; hiç ayarı olmayan bir uygulama önceki gibi
build edilir. Ayarlar kaydedildikten sonra kuyruğa giren deploy'lara uygulanır.

| Alan | Anlamı |
|---|---|
| `root_directory` | Build edilen alt dizin (monorepo), ör. `apps/web`. Build context'i ve algılama bu dizindir; `Dockerfile` da burada aranır. Göreli olmalı: başta `/` ve `..` yok; repo dışına çıkan symlink reddedilir |
| `framework` | Boş = otomatik algılama. Bir preset kimliği (aşağıdaki tablo) ya da tür: `node`, `go`, `python`, `ruby`, `java`, `static`, `dockerfile`. Verildiğinde algılamanın ve repodaki `Dockerfile`'ın önüne geçer |
| `install_command` | Bağımlılık kurulumu (ör. `pnpm install --filter web...`). Projenin tamamı kopyalandıktan sonra, cache mount'la çalışır |
| `build_command` | Build adımı (ör. `npm run build:prod`). Python'da çalışma imajında `USER`'dan önce (ör. `collectstatic`); Go'da `go build`'in yerine geçer ve binary'yi `/out/app`'e yazmalıdır |
| `start_command` | Başlatma komutu; `sh -c` ile çalışır, `$PORT` genişler, `Procfile`'ın önüne geçer. Node preset'lerinde build imajında, tüm `/app` ile çalışır. Go'da (distroless, kabuk yok) boşluklardan bölünüp `ENTRYPOINT` olur |
| `output_directory` | Statik build'in sunulacağı dizin (`root_directory`'ye göre), ör. `dist`, `build`, `site/out`. nginx-unprivileged ile sunulur |
| `node_version` | Node imajı: `20` → `node:20-alpine`, `20.18`, `lts`. Boş = `22` |

Komutlar tek satır olmalıdır (en çok 1024 karakter, satır sonu ve sondaki `\` reddedilir): üretilen
Dockerfile'a yeni talimat eklenemez. Repo kendi `Dockerfile`'ını getiriyorsa komut ayarları uygulanmaz,
build logunda uyarı çıkar; `framework` verilirse platform Dockerfile üretir.

**Framework preset'leri** (`package.json` bağımlılıklarından, bu sırayla algılanır):

| Framework | Kimlik | Algılama | Build | Çalıştırma |
|---|---|---|---|---|
| Next.js | `nextjs` | `next` | `next build` (`.next/cache` cache mount'ta) | `output: 'standalone'` → yalnızca `.next/standalone` + `static` kopyalanır, `node server.js`; `output: 'export'` → `out/` nginx'te; yoksa `next start -p $PORT` |
| Nuxt | `nuxt` | `nuxt` | `nuxt build` | `.output/` tek başına kopyalanır, `node .output/server/index.mjs`; build `nuxt generate` ise `.output/public` nginx'te |
| SvelteKit | `sveltekit` | `@sveltejs/kit` | `vite build` | `adapter-node` → `node build/index.js`; `adapter-static` → `build/` nginx'te; `adapter-auto` açıklayıcı hatayla reddedilir |
| Remix | `remix` | `@remix-run/dev` / `serve` / `node` | `build` script'i | `start` script'i, yoksa `remix-serve build/server/index.js` |
| React Router | `react-router` | `@react-router/dev` | `react-router build` | `start` script'i, yoksa `react-router-serve`; `ssr: false` → `build/client` nginx'te |
| Astro | `astro` | `astro` | `astro build` | `@astrojs/node` → `node dist/server/entry.mjs`; yoksa `dist/` nginx'te |
| NestJS | `nestjs` | `@nestjs/core` | `nest build` | `start:prod` script'i, yoksa `node dist/main` |
| Vite | `vite` | `vite` | `vite build` | `dist/` nginx'te (`start` script'i geliştirme sunucusu değilse Node sunucusu) |
| Create React App | `create-react-app` | `react-scripts` | `react-scripts build` | `build/` nginx'te |
| Express | `express` | `express` | `build` script'i (varsa) | `start` script'i, yoksa `server.js` / `index.js` |
| Node.js | `node` | diğer `package.json` | `build` script'i (varsa) | `start` script'i; yalnızca build varsa `dist/`, `build/`, `out/`, `public/`'ten statik |

Tüm Node sunucuları `PORT=8080`, `HOST=0.0.0.0`, `NODE_ENV=production` ve `USER 1000:1000` ile çalışır; statik
çıktılar `nginxinc/nginx-unprivileged` (uid 101) ile sunulur. Python (Django / FastAPI / Flask), Ruby (Rails / Rack),
Java (Spring Boot) ve Go da framework adını bildirir. Algılanan ad build logunda (`==> framework: Next.js`),
deployment'ın `framework` alanında ve ayarların `detected_framework` alanında görünür.

```bash
# Monorepo'daki Next.js uygulaması, Node 20 ile
curl -X PUT -H "Authorization: Bearer $TOKEN" localhost:8080/api/apps/web/settings \
  -d '{"root_directory":"apps/web","framework":"nextjs","node_version":"20"}'
# Gönderilmeyen alanlar korunur; "" varsayılana döndürür
curl -X PUT -H "Authorization: Bearer $TOKEN" localhost:8080/api/apps/web/settings -d '{"node_version":""}'
```

Bir dizin için üretilecek Dockerfile: `go run ./cmd/paas-dockerfile examples/nextjs-hello`. Örnekler:
[examples/nextjs-hello](../examples/nextjs-hello), [examples/vite-hello](../examples/vite-hello).


## Ortamlar ve deploy kontrolleri (Faz 17)

Vercel'deki gibi iki ortam var: **production** (production branch'ine push'lar ve production'a
yükseltilen deploy'lar) ve **preview** (diğer tüm branch'ler). Her deployment'ın `target` alanı hangi
ortamda çalıştığını, `origin` alanı nereden geldiğini (`git`, `redeploy`, `promote`, `hook`) gösterir.

**Ortama özel değişkenler.** `app_env` satırlarının bir hedefi var (migration 011):

| `target` | Kimler görür |
|---|---|
| `all` | iki ortam da (Faz 17 öncesi tüm satırlar ve hedefsiz yazılan değişkenler) |
| `production` | production deploy'ları |
| `preview` | preview deploy'ları; `git_branch` verilirse yalnızca o branch'in deploy'ları |

En özel olan kazanır: production için `all < production`, preview için
`all < preview < preview + branch`. Eski satırlar iki hedefe kopyalanmadı, `all` oldu: `all` ve
branch'siz satırların şifreleme AAD'si Faz 10'daki ile aynı (uygulama + değişken adı), böylece mevcut
şifreli değerler yeniden yazılmadan okunmaya devam ediyor. Diğer hedeflerde AAD hedefi ve branch'i de
içerir: bir preview değeri production satırına kopyalanırsa çözülmez. Anahtar rotasyonu
(`paas-envkey rotate`) her satırı kendi AAD'siyle yeniden şifreler.

```bash
# hedefsiz (geriye uyumlu): iki ortam; production/preview satırlarının yerini alır, branch'e özel olanlar kalır
curl -s -H "$H" -X PUT -d '{"SENTRY_DSN":"…"}' localhost:8080/api/apps/blog/env
# yalnızca production
curl -s -H "$H" -X PUT -d '{"target":"production","vars":{"DATABASE_URL":"postgres://prod…"}}' localhost:8080/api/apps/blog/env
# yalnızca feature/x preview'leri; null siler
curl -s -H "$H" -X PUT -d '{"target":"preview","git_branch":"feature/x","vars":{"FLAG":"on","ESKI":null}}' localhost:8080/api/apps/blog/env
curl -s -H "$H" localhost:8080/api/apps/blog/env
# {"keys":["DATABASE_URL","FLAG","SENTRY_DSN"],"vars":[{"key":"DATABASE_URL","target":"production",…},…]}
```

Hedefsiz `null` değişkeni her yerden (branch'e özel satırlar dahil) siler. Değişiklikler, eskisi
gibi, sonradan oluşan deploy'lara uygulanır: her deploy kendi değişmez `Secret`'ını alır.

**Promote (production'a yükselt).** `POST /api/apps/{name}/promote {"deployment_id"}` hazır bir
preview deploy'unu yeniden build etmeden production'a alır. Değişkenler ortama göre farklı ve deploy
başına değişmez bir `Secret`'a yazıldığı için alias'ı preview deploy'una çevirmek doğru olmazdı: o
deploy preview değişkenleriyle çalışıyor. Bu yüzden aynı commit için yeni bir deployment satırı açılır:
`target=production`, `origin=promote`, kaynağın digest ile sabitlenmiş imajı. Worker imajı hazır
bulunca build adımını atlar (`==> reusing image … (no rebuild)`), production değişkenleriyle yeni bir
`Secret` ve kendi nesnelerini oluşturur, hazır olunca production alias'ını taşır. Branch'in preview
alias'ı preview build'inde kalır. Kaynak zaten production ortamındaysa (ör. eski bir production
deploy'u) yeni deploy gerekmez, alias rollback'teki gibi anında taşınır (`"mode":"alias"`, `200`);
diğer durumda yanıt `202` ve `"mode":"deployment"`'dır.

Bir commit'in ikinci ve sonraki deploy'ları **nesil** (`generation`) numarası alır, adları ve URL'leri
çakışmaz: ilk deploy `d-<sha7>` / `https://<sha7>-<app>.<domain>`, n'inci kopya `d-<sha7>-<n>` /
`https://<sha7>-<n>-<app>.<domain>`. Push'lar her zaman 0. nesli bulur, yeniden teslim yine idempotenttir.

**Redeploy.** `POST /api/apps/{name}/deployments/{id}/redeploy` bitmiş bir deploy'u aynı commit, branch ve
ortamla yeniden çalıştırır (`202`). Varsayılan olarak imaj yeniden kullanılır; `{"use_cache": false}`
yeniden build ettirir. İmajı registry'den silinmiş (emekli ya da başarısız; Faz 30) bir deploy'un redeploy'u
imajı yeniden kullanamaz ve commit'i yeniden build eder. Kuyrukta ya da çalışmakta olan bir deploy için `409` döner.

**İptal.** `POST /api/deployments/{id}/cancel` (uygulamada member+). Durum makinesine `canceled` eklendi:

```
queued ──cancel──► canceled              (anında; hiçbir nesne oluşmadı)
building / deploying ──cancel──► cancel_requested_at → worker'ın heartbeat döngüsü
                                   context'i iptal eder → canceled (nesneler temizlenir)
```

Kuyruktaki deploy hemen `canceled` olur (`200`). Çalışan deploy'da istek veritabanına yazılır (`202`);
worker heartbeat'te bayrağı görüp çalışmayı durdurur (heartbeat kapalıysa da 2 sn'de bir bakılır).
Aynı anda `MarkReady`'ye ulaşan bir deploy alias almadan `canceled` biter; kısmen oluşmuş Kubernetes
nesneleri temizlikçi tarafından silinir. Bitmiş bir deploy için `409`. `Finished()` `canceled`'ı da
son durum sayar; GitHub commit status'u `error` ("Deploy iptal edildi") olur.

**Deploy hook'ları.** Uygulama başına gizli URL'ler: bir CMS ya da cron çağırınca yapılandırılan
branch'in son commit'i deploy edilir. Token yalnızca oluşturma yanıtında görünür, veritabanında
SHA-256'sı saklanır. Tetikleme URL'si kimlik doğrulama istemez, token'ın kendisi kimliktir. Hook'ları
listelemek, oluşturmak ve silmek member+ ister.

```bash
curl -s -H "$H" -d '{"name":"cms","branch":"main"}' localhost:8080/api/apps/blog/hooks
# {"id":1,"name":"cms","branch":"main","prefix":"paas_hook_AbC123","token":"paas_hook_…","path":"/api/hooks/deploy/paas_hook_…"}
curl -s -X POST localhost:8080/api/hooks/deploy/paas_hook_…
```

Branch head'i GitHub App üzerinden okunur (`BranchHead`); App yapılandırılmamışsa tetikleme `501`
döner. Yeni commit her zamanki gibi kuyruğa girer (`201`). Head zaten deploy edildiyse yeniden build
edilir (hook'lar genelde repo dışındaki içerik değişince çağrılır); o deploy hâlâ sürüyorsa onu döner
(`200`). Bilinmeyen token `404`, okunamayan branch `502`.

**Build'i atlama.** Push'un head commit mesajında (başlık ya da gövde; harf büyüklüğüne bakılmaz)
`[skip deploy]` veya `[skip ci]` geçiyorsa webhook deploy oluşturmaz: `202 {"result":"ignored"}` döner
ve loglanır; branch önceki deploy'unda kalır. Deploy hook'ları ve redeploy bu kurala bakmaz.


## Ortam değişkenlerinin şifrelenmesi (Faz 10)

Uygulama env değerleri Postgres'te AES-256-GCM ile şifreli durur (`internal/secret`). Her değer
rastgele 12 baytlık nonce ile ayrı şifrelenir ve `v1:<anahtar kimliği>:<base64(nonce|şifreli metin)>`
olarak `app_env.value` sütununa yazılır; `key_id` sütunu (migration 005) anahtarı gösterir, `NULL`
düz metin demektir. Ek doğrulama verisi (AAD) uygulama kimliği + değişken adıdır: bir şifreli değer
başka bir uygulamaya ya da değişkene kopyalanırsa çözülemez. Şifreleme store katmanında yapılır;
API, arayüz ve deployer değişmedi, Kubernetes `Secret`'ı yine çözülmüş değerlerle oluşur.

```bash
go run ./cmd/paas-envkey            # yeni anahtar yazdırır (stderr: anahtar kimliği)
go run ./cmd/paas-envkey status     # anahtar kimliği başına değer sayısı (PAAS_DATABASE_URL)
go run ./cmd/paas-envkey rotate     # düz metinleri şifreler, eski anahtardakileri yenisine taşır
```

| Değişken | Açıklama |
|---|---|
| `PAAS_ENV_KEY` | base64 32 bayt; kimlik `sha256(anahtar)`'ın ilk 8 hex karakteri ya da açıkça `kimlik:base64` |
| `PAAS_ENV_OLD_KEYS` | virgülle ayrılmış eski anahtarlar; yalnızca çözmek için kullanılır |

- **Açılışta:** kontrol düzlemi migration'lardan sonra düz metin değerleri şifreler ve eski
  anahtarlardaki değerleri güncel anahtarla yeniden şifreler (tek transaction, idempotent; güncel
  anahtardaki satırlara dokunulmaz). Tanınmayan bir anahtar kimliği varsa açılmaz.
- **Rotasyon:** yeni anahtarı `PAAS_ENV_KEY`'e, eskisini `PAAS_ENV_OLD_KEYS`'e koy, yeniden başlat
  (ya da `paas-envkey rotate` çalıştır); `status` eski anahtarda değer kalmadığını gösterince eski
  anahtarı listeden çıkar. Kontrol düzleminin tek kopyası varsayılır: rotasyon sırasında eski
  yapılandırmayla çalışan bir kopya yeni değerleri okuyamaz.
- **Anahtar yoksa:** veritabanında şifreli değer varsa kontrol düzlemi açılmayı reddeder
  (değerler okunamaz, yeni düz metin yazmak karışıklık yaratır). Şifreli değer yoksa uyarı loglar
  ve değerleri düz metin saklar — yalnızca yerel geliştirme içindir. Anahtar kaybı, şifreli env
  değerlerinin kaybıdır; anahtarı veritabanı yedeğinden ayrı yedekle.


## Deploy nasıl çalışır

`PAAS_DEPLOYER=kubernetes` iken worker, build'in ürettiği digest'li imajı kümeye koyar:

```
namespace app-<app>          (managed-by=paas, ResourceQuota + LimitRange + NetworkPolicy "paas")
├── Secret     d-<sha7>-env  değişmez; deploy anındaki ortam değişkenleri
├── Deployment d-<sha7>      1 replika, PORT=8080, PAAS_APP/COMMIT_SHA/BRANCH, envFrom secret
├── Service    d-<sha7>      ClusterIP :80 → :8080
├── Ingress    d-<sha7>      <sha7>-<app>.<domain> → Service d-<sha7>
└── Ingress    alias-<etiket> <app>.<domain> (production), <branch>-<app>.<domain> (preview)
```


## Yönlendirme ve rollback (Faz 4)

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
Wildcard alınamayan adlarda (ör. DuckDNS) `PAAS_INGRESS_CERT_ISSUER=letsencrypt-http01` ile her
route kendi `<ingress>-tls` Secret'ını adlandırır ve cert-manager host başına HTTP-01 sertifikası
alır. Rollback yalnızca backend'i değiştirdiği için sertifika yeniden alınmaz; emekliye ayrılan
deploy'un ve kaldırılan alias'ın Secret'ı silinir. Let's Encrypt kayıtlı alan adı başına haftada
50 yeni sertifika verir (her yeni deploy adresi bir sertifika); yeni bir deploy adresine ilk HTTPS
isteği sertifika çıkana kadar ~10–30 s bekleyebilir.

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


## Yaşam döngüsü ve temizlik (Faz 7)

Deploy'lar değişmezdir ama sonsuza kadar çalışmaz. Temizlik döngüsü (`PAAS_GC_INTERVAL`,
varsayılan 10 dk; bir deploy bitince de tetiklenir) her uygulama için:

1. Herhangi bir alias'ın gösterdiği deploy'a **asla dokunmaz**.
2. Production branch'inin en yeni `PAAS_KEEP_PRODUCTION` (5) hazır deploy'unu rollback hedefi olarak tutar.
3. Alias'ı kalmamış preview deploy'larını, bitişlerinden `PAAS_PREVIEW_TTL` (72 saat) sonra emekliye ayırır.
4. Emekli ve başarısız deploy'ların Kubernetes nesnelerini siler (Deployment silinince Service,
   Secret ve Ingress de gider); silme başarısız olursa bir sonraki turda tekrar dener.
5. Ardından bu deploy'ların imajlarını registry'den siler (Faz 30, aşağıda).

Emekli (`retired`) bir deploy'a rollback yapılamaz (`409`); aynı commit tekrar push'lanırsa
yeniden build edilir. Branch GitHub'da silinince ya da PR kapanınca o branch'in preview alias'ı
hemen kalkar ve deploy'ları emekliye ayrılır; production branch'i bu yolla asla temizlenmez.

Çökme sonrası kurtarma: worker çalıştığı deploy için heartbeat yazar. `PAAS_WORKER_STALE_AFTER`
(2 dk) boyunca heartbeat gelmezse deploy yeniden kuyruğa alınır; ikinci denemede de sahipsiz
kalırsa `failed` olur. Ayrıntılar ve tüm arıza durumları: [docs/FAILURE-SCENARIOS.md](FAILURE-SCENARIOS.md).

| Değişken | Açıklama |
|---|---|
| `PAAS_KEEP_PRODUCTION` | rollback için tutulan production deploy sayısı (varsayılan `5`) |
| `PAAS_PREVIEW_TTL` | alias'sız preview'in emekliye ayrılma süresi (varsayılan `72h`) |
| `PAAS_PREVIEW_ALIAS_TTL` | hiç güncellenmeyen preview alias'ının kaldırılma süresi (varsayılan `0` = asla) |
| `PAAS_GC_INTERVAL` | temizlik turu aralığı (varsayılan `10m`) |
| `PAAS_WORKER_STALE_AFTER` | heartbeat'siz kalan deploy'un sahipsiz sayılma süresi (varsayılan `2m`) |
| `PAAS_IMAGE_GC` | `false` → imajlar registry'den silinmez (varsayılan `true`; `dryrun` builder'da kapalı) |

### İmaj saklama (Faz 30)

Registry'de tutulan imajlar, platformun tuttuğu deploy'ları izler; repo başına bir sayı sınırı yoktur.
Canlı deploy'un, rollback hedeflerinin (`PAAS_KEEP_PRODUCTION`), etkin preview'lerin ve kuyrukta ya da
build/deploy aşamasındaki deploy'ların imajları **asla silinmez**; ne kadar preview push'lanırsa push'lansın
rollback hedefi çekilebilir kalır.

- Bir deploy emekliye ayrılınca, başarısız olunca ya da iptal edilince temizlik önce Kubernetes nesnelerini,
  sonra imajını siler (`DELETE /v2/<repo>/manifests/<digest>`). Silinen satır deploy logunda görünür:
  `==> image deleted from the registry: …@sha256:…`.
- Karar **digest başına** verilir: imajı yeniden kullanan redeploy ve promote'lar, ya da aynı digest'i üreten iki
  build aynı manifest'i paylaşır. Tutulan herhangi bir deploy o manifest'i gösteriyorsa imaj kalır; sonuncusu
  da emekliye ayrılınca silinir.
- Registry geçici olarak erişilemezse deploy "temizlendi" sayılmaz, bir sonraki turda tekrar denenir. Silmeye
  izin vermeyen bir registry (`UNSUPPORTED`) için imaj bırakılır ve uyarı loglanır.
- Proje silinince projenin registry'deki bütün imajları (build önbelleği dahil) silinir; ad ancak bundan sonra
  yeniden alınabilir.

Registry'ye göre:

| Registry | Gerekenler |
|---|---|
| Yerel `registry:2` (`docker compose`) | `REGISTRY_STORAGE_DELETE_ENABLED=true` (compose dosyasında açık). Silme manifest'i kaldırır; disk alanı `docker compose exec registry registry garbage-collect /etc/docker/registry/config.yml` ile geri gelir |
| k3d registry'si (`scripts/k3d-up.sh`) | Silme kapalıdır; imajlar kalır, temizlik uyarı loglar |
| Amazon ECR | Düğüm rolünde `ecr:BatchDeleteImage` (Terraform ekler). Yaşam döngüsü kuralı yalnızca **etiketsiz** imajları `ecr_untagged_days` (7) gün sonra siler; etiketli imajları kontrol düzlemi siler |
| GHCR, Docker Hub, Harbor | Docker config'inde (`$DOCKER_CONFIG/config.json`, satır içi `auths`) silme yetkisi olan bir token |

Kimlik bilgileri builder'ın kullandığı Docker config'inden her çağrıda yeniden okunur (AWS'de
`paas-ecr-auth` zamanlayıcısının yenilediği token hemen kullanılır); Basic (ECR) ve Bearer token
(GHCR, Docker Hub) kimlik doğrulaması desteklenir. Credential helper (`credsStore`) okunmaz.


## Özel alan adları (Faz 12)

Bir uygulamaya kendi alan adınızı (ör. `www.ornek.com`) bağlayabilirsiniz. Alan adı DNS ile
doğrulandıktan sonra uygulamanın **production** deploy'unu sunar ve production alias'ını izler:
yeni bir production deploy'u da, rollback da alan adını aynı anda taşır.

Ekleme: web arayüzünde uygulama sayfasındaki **Custom domains** bölümü ya da

```bash
curl -s -H "Authorization: Bearer $TOKEN" -d '{"hostname":"www.ornek.com"}' \
  https://<domain>/api/apps/blog/domains
```

Yanıt (`dns_records`) oluşturulacak kayıtları listeler. **Birini** oluşturun:

| Tür | Ad | Değer | Ne zaman |
|---|---|---|---|
| `CNAME` | `www.ornek.com` | `blog.<domain>` (ya da `<domain>`) | alt alan adları için önerilen yol; trafiği de yönlendirir |
| `TXT` | `_paas-challenge.www.ornek.com` | yanıttaki `verification_token` | CNAME olamayan kök alan adları (`ornek.com`) için; ayrıca `A` (ya da `ALIAS`) kaydı `blog.<domain>` ile aynı adrese |

Alan adı kuralları: tam nitelikli ad (en az iki etiket), harf büyüklüğü önemsizdir (ad normalize edilir), uluslararası adlar
punycode (`xn--…`) biçiminde yazılır, joker (`*.`) ve platform alan adının altındaki adlar kabul
edilmez. Bir ad tüm platformda tektir; uygulama başına en fazla 20 alan adı.

Durumlar:

| Durum | Anlamı |
|---|---|
| `pending` | DNS kaydı henüz bulunamadı; `error` neyin eksik olduğunu söyler. Her `PAAS_DOMAIN_CHECK_INTERVAL`'da (1 dk; bir günden eski olanlar saatte bir) yeniden denenir |
| `verified` | DNS doğrulandı, `Ingress` oluşturuldu; sertifika bekleniyor (ya da henüz production deploy'u yok) |
| `active` | Yönlendiriliyor ve sertifikası hazır. Her `PAAS_DOMAIN_RECHECK_INTERVAL`'da (1 sa) yeniden doğrulanır |
| `error` | Doğrulanmış bir alan adının DNS kaydı kayboldu. `PAAS_DOMAIN_GRACE` (72 sa) boyunca sunulmaya devam eder, sonra `Ingress`'i kaldırılır; kayıt düzelince kendiliğinden geri gelir |

Bekleme süresinin nedeni: geçici bir DNS sorunu ya da kısa bir sağlayıcı değişikliği canlı bir
siteyi anında düşürmemeli; kalıcı olarak başka yere taşınan bir alan adı ise süre sonunda bırakılır.
"Check now" düğmesi ve `POST /api/apps/{name}/domains/{hostname}/verify` döngüyü beklemeden denetler.

**Sertifika:** platform adları wildcard sertifikayla (DNS-01) sunulur; özel alan adları bu
bölgenin dışında olduğundan her biri kendi Let's Encrypt sertifikasını **HTTP-01** ile alır.
Kontrol düzlemi alan adının `Ingress`'ine `cert-manager.io/cluster-issuer: letsencrypt-http01`
(`PAAS_CUSTOM_DOMAIN_ISSUER`) anotasyonunu ve alan adına özel bir TLS Secret'ı yazar;
cert-manager `Certificate`'i oluşturur, doğrulamayı Traefik'in `web` giriş noktasındaki geçici bir
`Ingress` ile yapar ve sertifikayı süresi dolmadan yeniler. HTTP→HTTPS yönlendirmesi
`/.well-known/acme-challenge/` yolunu hariç tutar ([infra/k8s/40-tls.yaml](../infra/k8s/40-tls.yaml)).
Sertifika hazır olana kadar (genellikle bir dakikadan kısa) HTTPS isteklerine platformun
varsayılan sertifikası döner ve tarayıcı uyarı verir; durum bu sürede `verified` kalır.
Alan adı silinince `Ingress`, `Certificate` ve TLS Secret'ı da silinir.
`PAAS_INGRESS_TLS=false` iken alan adı düz HTTP ile sunulur, sertifika istenmez.

`PAAS_DOMAIN_VERIFY=skip` DNS denetimini atlar ve eklenen her alan adını hemen yönlendirir.
**Yalnızca geliştirme içindir** (ör. genel DNS'i olmayan bir k3d kümesi); üretimde başkasına ait
bir adın bu platforma bağlanmasına izin verir.

| Değişken | Açıklama |
|---|---|
| `PAAS_DOMAIN_CHECK_INTERVAL` | bekleyen / doğrulanmış / hatalı alan adlarının denetim aralığı (`1m`) |
| `PAAS_DOMAIN_RECHECK_INTERVAL` | `active` alan adlarının yeniden doğrulama aralığı (`1h`) |
| `PAAS_DOMAIN_GRACE` | DNS'i bozulan alan adının sunulmaya devam ettiği süre (`72h`) |
| `PAAS_DOMAIN_VERIFY` | `dns` (varsayılan) ya da `skip` (yalnızca geliştirme) |
| `PAAS_CUSTOM_DOMAIN_ISSUER` | alan adı sertifikaları için ClusterIssuer (`letsencrypt-http01`) |


## Süreç tipleri (Faz 20)

Vercel sunucusuzdur: uzun yaşayan süreç, arka plan işçisi, kalıcı WebSocket sunucusu yoktur ve cron yalnızca bir HTTP
ucunu çağırır. paas gerçek konteynerler çalıştırdığı için aynı repo ve aynı imajdan **Discord botunu, kuyruk işleyicini,
zamanlanmış işini ve WebSocket sunucunu da** deploy eder.

```yaml
# paas.yaml (repo kökünde; kök dizin ayarı varsa orada). paas.yml ya da paas.json da olur.
processes:
  web:    { command: "node server.js" }          # isteğe bağlı; yoksa algılanan başlatma komutu
  worker: { command: "node worker.js", replicas: 2, previews: true }
  bot:    { command: "python bot.py" }            # previews varsayılanı false
crons:
  - name: cleanup
    schedule: "*/15 * * * *"                       # UTC; @hourly, @daily, @weekly, @monthly, @yearly da olur
    command: "node scripts/cleanup.js"
```

- **Öncelik:** `paas.yaml` > `Procfile` (`web:` başlatma komutu, diğer satırlar 1 kopyalı worker, `release:` yok sayılır) > yalnızca web.
  Web komutunda proje ayarlarındaki başlatma komutu `paas.yaml`'dan önce gelir. Komut bir metinse imajın kabuğuyla (`sh -c`)
  çalışır, liste ise doğrudan; Go imajlarında kabuk olmadığı için metin boşluklardan bölünür (ikili `/app`'tedir).
- **Web'siz uygulama:** `web: none` yaz. `web` hiç geçmiyorsa ve başlatma komutu algılanamıyorsa ama worker varsa uygulama
  yine web'siz deploy edilir (build logu bunu söyler). Web'siz bir deploy'un URL'i, Service'i ve Ingress'i yoktur; worker'ları
  ayağa kalkınca hazırdır ve rollback için alias'ları tutulur.
- **Hangi nesil çalışır:** worker'lar ve cron'lar yalnızca production alias'ının gösterdiği deploy'da çalışır. Preview ortamındaki
  bir branch preview'inde yalnızca `previews: true` olanlar çalışır; diğer bütün deploy'larda worker'lar 0 kopyadır ve cron'lar
  askıdadır. Böylece iki değişmez deploy aynı kuyruğu tüketmez, aynı cron iki kez tetiklenmez; **rollback** worker ve cron'ları da
  eski sürüme taşır. Yeni production deploy'unun worker'ları alias'tan önce başlatılır ve 10 saniye çökmeden ayakta kalmaları
  beklenir; çöken worker deploy'u düşürür. Eski neslin worker'ları birkaç saniye (alias senkronuna kadar) yenilerle çakışabilir.
- **Ölçekleme:** `paas ps scale <app> worker=3` (ya da arayüzde "Ölçekle") uygulama genelinde production için paas.yaml'ı geçersiz kılar
  ve sonraki deploy'larda da geçerlidir; `worker=default` paas.yaml'a döner. Web süreci sıfıra ölçeklemeyle yönetilir; worker'lar asla uyutulmaz.
- **Cron:** `paas cron run <app> cleanup` işi hemen başlatır. Aynı anda tek çalışma (`Forbid`), 5 dakika gecikme payı, en fazla 1 saat süre.
- **Loglar:** `paas logs --runtime --process worker <app> <deploy>`; bir cron'un adı son çalışmasının logunu verir.
- **Kota:** her süreç kopyası ve çalışan her cron işi namespace'in pod kotasından (`PAAS_APP_QUOTA_PODS`, varsayılan 20) yer;
  kota aşımı deploy'u açık bir hatayla düşürür.
- **Ortam:** her süreç aynı env Secret'ını ve platform değişkenlerini alır, ayrıca `PAAS_PROCESS=<ad>` (web'de `web`).

Örnekler: [`examples/websocket-chat`](../examples/websocket-chat) ve [`examples/worker-queue`](../examples/worker-queue).


## Kademeli yayın (Faz 21)

Vercel'de yalnızca Enterprise'da olan "rolling releases"in karşılığı. Değişmez deploy'lar, alias
olarak production, anında rollback ve deploy başına istek metrikleri (Faz 19) zaten vardı; Faz 21
bunları birleştirir: yeni bir production deploy'u önce trafiğin bir payını (varsayılan %10) alır, metrikleri
mevcut production ile karşılaştırılır ve yayın ya kendiliğinden ilerler ya da kendiliğinden geri alınır.

| Biçim | Davranış |
|---|---|
| `instant` (varsayılan) | Eskisi gibi: hazır olan production deploy'u alias'ı hemen alır |
| `guarded` | Alias hemen taşınır; yeni production izleme süresi (varsayılan 10 dk) boyunca izlenir, bozulursa önceki deploy'a otomatik dönülür |
| `canary` | Production alias'ı yerinde kalır; yeni deploy adımlarla (varsayılan %10 → %50 → %100, adım 5 dk) trafiğin payını alır, %100'de alias taşınır |

**Karar kuralları** (`internal/rollout`, saf ve tablo testli `Decide` fonksiyonu; denetleyici 30 sn'de bir çalışır):

1. Yeni deploy adımda en az `min_requests` (50) istek aldıysa, adım bitmesini beklemeden geri alınır:
   5xx oranı `max_error_pct`'yi (%5) aşarsa; production'ın oranından `max_error_increase_pct`
   puandan (2) fazla kötüyse; p95'i `max_p95_ms`'yi (varsayılan kapalı) ya da production p95'inin
   `max_p95_factor` katını (2×; en az 25 ms fark) aşarsa.
2. Daha az istek varsa yalnızca açık kanıt sayılır: en az 5 yanıt 5xx ve oran eşiğin üstünde.
3. **Az trafikli projeler:** eşik kadar istek gelmezse ve hata kanıtı yoksa adım süre dolunca ilerler
   (beklemek hiçbir zaman kanıt üretmez). Bunu istemeyenler `min_requests`'i düşürebilir ya da `guarded` kullanabilir.
4. Metrik yoksa (iki tarafta da istek yok ama production'ın yayından önce trafiği vardı) yayın bekler,
   3 adım süresi boyunca metrik gelmezse durdurulur; production değişmez.
5. Aksi halde adım geçer: sıradaki ağırlık ya da son adımda production alias'ının taşınması.

Metrikler dakikalık toplandığından adım en az 2 dakikadır; karar penceresi adımın başladığı
dakikadan sonraki tam dakikalardır (ağırlık değişmeden önceki trafik karışmaz).

**Yönlendirme.** Düz `networking/v1` Ingress trafik bölemez ve Traefik'in Ingress sağlayıcısı
`TraefikService`'e bağlanmayı reddeder (Traefik 3.6'da denendi: `Resource backends are not supported`).
Bu yüzden canary süresince production adı ve özel alan adları için Traefik CRD'leriyle bir katman eklenir:
ağırlıklı `TraefikService rollout` (`d-<eski>` 100-w, `d-<yeni>` w) ve host başına yüksek öncelikli
(`100000`) `IngressRoute`. Alias Ingress'i hiç silinmez; yayın bitince katman silinir ve Ingress
(promote edildiyse yeni deploy'u gösterir) devam eder. TLS aynıdır (websecure + wildcard, ya da
`PAAS_INGRESS_CERT_ISSUER` / özel alan adı Secret'ı); ACME HTTP-01 yolu katmandan hariç tutulur.
Ağırlıklı servisin çocukları Traefik metriklerinde `<ns>-d-<sha7>-http@kubernetescrd` olarak görünür
(canlı doğrulandı); analitik ve sıfıra ölçekleme bu seriyi de deploy'a bağlar. Yayının iki tarafı da
uyutulmaz ve emekliye ayrılmaz.

**Boru hattı.** Karar `MarkReady` transaction'ında verilir: canary'de production alias'ı taşınmaz
(branch preview'ı taşınır), deploy loguna `==> canary: %10 trafik (…)` yazılır. Daha yeni bir production
deploy'u süren yayının yerini alır (`superseded`; yeni yayın kararlı production'dan başlar). Elle
rollback ya da production deploy'unun promote edilmesi yayını durdurur (`aborted`); preview'in promote
edilmesi açık bir istek olduğu için canary'ye girmeden hemen geçer. Geri alınan canary `ready` kalır,
kendi adresinden açılır; listede "Canary geri alındı" olarak işaretlenir, GitHub'a `paas/deploy/rollout`
bağlamında commit status'u gider.

```bash
# Ayarlar (gönderilmeyen alanlar korunur)
curl -X PUT -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/rollout-settings \
  -d '{"mode":"canary","steps":[10,50,100],"step_seconds":300,"max_error_pct":5,"max_p95_factor":2}'
# Durum: etkin yayın, iki tarafın istek / 5xx / p95'i, sıradaki karar; son yayınlar
curl -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/rollout
# Elle: promote (hemen tamamla) | pause | resume | rollback | abort
curl -X POST -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/rollout/pause

paas rollout settings blog --mode canary --steps 10,50,100 --step 5m --max-p95-ms 800
paas rollout status blog
paas rollout promote blog
```

| Değişken | Açıklama |
|---|---|
| `PAAS_ROLLOUT_INTERVAL` | denetleyicinin değerlendirme aralığı (varsayılan `30s`, en az `5s`) |

Denetleyici her kontrol düzlemi kopyasında çalışır; yayınlar `FOR UPDATE SKIP LOCKED` ile alınır,
her geçiş (durum, adım) üzerinde compare-and-set'tir: aynı anda gelen kullanıcı işlemi ya da yeni
deploy kazanır, eski karar düşer. Küme RBAC'ına `traefik.io/traefikservices` eklendi.


## Veritabanları (Faz 22)

Vercel veritabanını kendisi barındırmaz, kullanıcıyı pazar yerindeki ortaklara yollar. paas zaten
Kubernetes'te çalıştığı için bir projeye tek tıkla PostgreSQL ya da Redis eklenir, bağlantı değişkenleri
deploy'lara kendiliğinden girer ve — asıl özellik — **her preview branch'i production veritabanının kendi
kopyasıyla** çalışır: bir pull request gerçekçi veriyle, production'a dokunmadan denenir.

| | PostgreSQL 16 | Redis 7 |
|---|---|---|
| Kubernetes | `StatefulSet addon-<ad>` + `Service` + PVC `addon-<ad>-data` (+ `-backups`) | `StatefulSet addon-<ad>` + `Service` + PVC |
| Güvenlik | UID 70, salt okunur kök, yetenekler kapalı; deploy'lar `app` rolüyle bağlanır, süper kullanıcı yalnızca platformun işlerinde | UID 999, `requirepass`, AOF, `noeviction` |
| Değişkenler | `DATABASE_URL`, `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` | `REDIS_URL` |
| Önizlemeler | `copy` (varsayılan), `empty`, `shared` | paylaşılır |
| Yedek | günlük `pg_dump -Fc` (son 7), elle yedek, geri yükleme | — |

Boyutlar: `hobby` (256Mi bellek, 1Gi disk), `standard` (512Mi, 5Gi), `pro` (1Gi, 20Gi); plan büyütülünce sunucu
yeniden başlar, disk genişletilir (local-path genişletemez: uyarı gösterilir), disk asla daraltılmaz. Uygulama
başına en fazla 5 eklenti. Namespace kotası eklentilerle büyür (deploy'ların payı yenmez) ve
`NetworkPolicy paas-addons` eklentilere yalnızca aynı namespace'teki pod'ların, yalnızca 5432 / 6379'dan
erişmesine izin verir.

**Değişkenler.** Hazır eklentiler değişkenlerini `DeploymentEnv` ile verir; **aynı adla tanımladığın kendi
değişkenin kazanır** (ör. dışarıdaki bir veritabanına `DATABASE_URL` vermek için). Varsayılan adlı eklenti
(`db`, `cache`; yoksa türünün en eskisi) düz adları, başka adlı her eklenti ayrıca önekli adları verir
(`analytics` → `ANALYTICS_DATABASE_URL`, `ANALYTICS_PGHOST`, …). Eklenti eklendikten sonra çalışan deploy'lar
değişkenleri yeniden deploy edilince alır.

**Preview kopyası.** `copy` modunda bir branch'in ilk preview deploy'u build'den önce kopyayı ister, kopya build
sürerken çalışır, deploy onu bekler:

```
==> veritabanı kopyası: db → preview_feature_x_1a2b3c4d (production'dan kopyalanıyor, anonimleştirilecek)
...build...
==> veritabanı kopyası: db → preview_feature_x_1a2b3c4d kopyalanıyor…
==> veritabanı kopyası: db hazır (8.6 MiB, anlık görüntü 2026-10-07 05:27:04 UTC, 2 anonimleştirme kuralı)
```

Kopya aynı Postgres sunucusunda `preview_<branch>` adlı (uzun / değişen adlarda özet ekli) ayrı bir veritabanı
ve ona sahip ayrı bir roldür; `pg_dump -Fc` production'dan okur, `pg_restore --role=…` kopyaya yazar
(`CREATE DATABASE … TEMPLATE` kullanılmaz: production'daki bağlantıları kesmek gerekirdi). Preview production'a,
production preview'e bağlanamaz. Production `PAAS_ADDON_COPY_MAX_SIZE`'ı (5Gi) aşıyorsa ya da kopya diski
%80'in üstüne dolduracaksa boş veritabanı açılır ve deploy loguna uyarı düşer. Kopya başarısızsa deploy
nedeniyle `failed` olur; sonraki push tekrar dener. Branch silinince / PR kapanınca, preview'leri TTL ile
emekliye ayrılınca kopya `DROP DATABASE … WITH (FORCE)` ile silinir. "Kopyayı yenile" production'dan yeniden alır
(önizlemenin yazdığı veriler gider). Modu `shared`'a çevirmek mevcut kopyaları siler; `copy` ↔ `empty` değişikliği
sonraki kopyalarda geçerli olur. `shared` modda önizlemeler production'a production'ın kimliğiyle bağlanır:
yalnızca önizleme kodunun production verisine yazmasında sakınca yoksa seç.

**Anonimleştirme** yalnızca kopyada çalışır. Kurallar yalnızca tanımlayıcıdır ve SQL'e sabit ifadelerle derlenir:

```
users.email: email       # user_<md5>@example.invalid (eşit değerler eşit kalır, benzersizlik korunur)
users.name: name         # User 1A2B3C
billing.cards.number: hash
*.phone: null            # phone sütunu olan her tablo
```

Stratejiler: `email`, `name`, `phone`, `hash`, `redact`, `empty`, `null` (NULL her zaman NULL kalır). Kurallar
süper kullanıcıyla, uygulamanın tetikleyicileri kapalıyken tek transaction'da çalışır. Daha fazlası için üyeler
**ek SQL ifadeleri** yazabilir (`anonymize_sql`): bunlar kurallardan sonra, kopyanın kendi rolüyle (production'a
erişemeyen, süper kullanıcı olmayan) ve psql ters bölü komutları olmadan çalışır.

```bash
# PostgreSQL ekle; hazır olunca deploy'lar DATABASE_URL alır
curl -X POST -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/addons \
  -d '{"kind":"postgres","plan":"hobby","preview_mode":"copy"}'
curl -X PATCH -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/addons/db \
  -d '{"anonymize":["users.email: email","*.phone: null"],"backup_keep":7}'
# Şifreyi görmek (üye; her görüntüleme sunucu loguna yazılır)
curl -X POST -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/addons/db/credentials/reveal

paas addons add blog postgres --preview-mode copy
paas addons set blog db --anonymize "users.email: email" --anonymize "*.phone: null"
paas addons branches blog db
paas addons reset blog db feature/x
paas addons backup blog db && paas addons backups blog db
paas addons restore blog db 12 --confirm db
paas addons rotate blog db            # yeni şifre; ardından production ve preview adresleri yeniden deploy edilir
paas addons rm blog db --confirm db   # veriler, kopyalar ve yedeklerle birlikte
paas db psql blog                     # kubectl port-forward + psql komutunu yazar
```

`paas db psql` bir tünel açmaz: eklentiler yalnızca küme içinden erişilebilir; komut, küme erişimi olan biri
için `kubectl -n app-blog port-forward svc/addon-db 15432:5432` ve şifreli `psql` komutunu yazar.

**Yedekler** (`PAAS_ADDON_BACKUP_SCHEDULE`, varsayılan her gün 03:00 UTC) eklentinin yedek diskine yazılır;
eklentinin kendi diskiyle aynı düğümdedir, düğüm kaybına karşı korumaz (S3 uyumlu hedef sonraki aşamada).
Geri yükleme, eklentinin adını yazarak onaylanan bir iştir ve tek transaction'dır (`pg_restore --clean
--single-transaction`): hata olursa production değişmez. **Şifre yenileme** Postgres'te `ALTER ROLE` işiyle,
Redis'te pod'un yeniden başlamasıyla uygulanır; açık bağlantılar sürer, yeni bağlantılar yeni şifreyi ister
(varsayılan olarak alias'ların gösterdiği deploy'lar hemen yeniden deploy edilir).

| Değişken | Açıklama |
|---|---|
| `PAAS_ADDON_STORAGE_CLASS` | eklenti disklerinin sınıfı (varsayılan `local-path`; `default` kümenin varsayılanı) |
| `PAAS_ADDON_POSTGRES_IMAGE` / `PAAS_ADDON_REDIS_IMAGE` | imajlar (`postgres:16-alpine`, `redis:7-alpine`) |
| `PAAS_ADDON_BACKUP_SCHEDULE` | günlük yedek zamanı, cron, UTC (`0 3 * * *`) |
| `PAAS_ADDON_COPY_MAX_SIZE` | bunun üstündeki production kopyalanmaz, boş veritabanı açılır (`5Gi`) |
| `PAAS_ADDON_COPY_TIMEOUT` | bir kopya işinin süresi (`10m`) |
| `PAAS_ADDON_INTERVAL` | uzlaştırma döngüsü aralığı (`10s`) |

Küme RBAC'ına `statefulsets` ve `persistentvolumeclaims` eklendi; `pods/exec` kullanılmaz (işlerin sonucu pod'un
sonlandırma mesajından okunur). `PAAS_DEPLOYER=dryrun` ile eklentiler küme olmadan hazır sayılır, kopyalar ve
yedekler veri olmadan başarılı olur (yerel geliştirme).


## Sıfıra ölçekleme (Faz 11)

`PAAS_SCALE_TO_ZERO_AFTER` (varsayılan `30m`) boyunca istek almayan deploy'lar 0 replikaya iner;
ilk istek gelince uyanır. İstek bekletilir, pod hazır olunca ona iletilir: kullanıcı hata sayfası
görmez, yalnızca ilk yanıt birkaç saniye gecikir (yerel k3d'de 4–7 s, bkz.
[MEASUREMENTS.md](MEASUREMENTS.md#6-sıfıra-ölçekleme-k3d)). Preview'ler ve production
alias'ı göstermeyen eski deploy'lar her zaman uyuyabilir; production deploy'u yalnızca
uygulama izin verirse:

```bash
curl -X PUT -H "Authorization: Bearer $PAAS_API_TOKEN" localhost:8080/api/apps/blog/scale-to-zero \
  -d '{"production": true}'
```

İzin geri alınırsa ya da uyuyan bir deploy'a rollback yapılırsa, ölçekleyici o production
deploy'unu bir sonraki turda kendisi uyandırır. Uyuyan deploy'lar API'de `"sleeping": true`
(`sleeping_since`), web arayüzünde **sleeping** etiketiyle görünür.

**Nasıl çalışır** (kontrol düzlemi aktivatör olarak; KEDA gerekmez):

1. **Boşta olma tespiti:** Ölçekleyici her turda (`PAAS_SCALE_INTERVAL`, varsayılan
   `min(süre/4, 30s)`) Traefik'in servis başına `traefik_service_requests_total` sayaçlarını
   okur (k3s'in Traefik'i, `:9100`; API sunucusunun pod proxy'si üzerinden; servis etiketleri
   [05-traefik-config.yaml](../infra/k8s/05-traefik-config.yaml) ile açılır, bkz. [Analitik](#analitik-faz-19)).
   Sayaç süre boyunca değişmemişse deploy boştadır. Her değişiklik (Traefik yeniden başlayınca
   sıfırlanma dahil) etkinlik sayılır; sayaçlar okunamazsa hiçbir şey uyutulmaz.
2. **Uyutma:** Deploy'un Service'inin selector'ı kaldırılır ve aynı Service'e kontrol
   düzleminin aktivatör portunu gösteren bir EndpointSlice (`d-<sha7>-activator`) eklenir;
   ardından Deployment 0 replikaya iner. Ingress'lere (deploy adresi, alias'lar, özel alan
   adları) dokunulmaz, bu yüzden alias uzlaştırma döngüsü uyku durumunu bozamaz.
3. **Uyandırma:** İstek Traefik → aktivatöre gelir. Aktivatör hostname'den Ingress'i ve
   Deployment'ı bulur, 1 replikaya çıkarır, hazır olmasını bekler (200 ms aralıkla; çöken pod
   hemen hata verir), Service'i pod'lara geri çevirir ve bekletilen isteği iletir. Aynı deploy
   için gelen eşzamanlı istekler tek bir uyandırmayı paylaşır.

| Değişken | Açıklama |
|---|---|
| `PAAS_SCALE_TO_ZERO_AFTER` | boşta kalma süresi (varsayılan `30m`, `0` kapatır) |
| `PAAS_SCALE_INTERVAL` | kontrol aralığı (boş = `min(süre/4, 30s)`) |
| `PAAS_ACTIVATOR_ADDR` | aktivatörün dinlediği adres (varsayılan `:8081`) |
| `PAAS_ACTIVATOR_IP` | Traefik'in aktivatöre ulaştığı IP; kümede pod IP'si (downward API), k3d'de `host.k3d.internal` adresi. Boşsa özellik kapalı kalır |
| `PAAS_ACTIVATOR_UPSTREAM` | `pod` (kümede, isteği doğrudan pod'a iletir) ya da `http://127.0.0.1:80` (kontrol düzlemi kümenin dışında; istek Traefik üzerinden yeniden gönderilir) |
| `PAAS_CONTROL_PLANE_NAMESPACE` | kümede: uygulama NetworkPolicy'leri kontrol düzlemi pod'larına izin verir (`pod` upstream'i için) |

Yerel k3d ile: `scripts/k3d-up.sh` gereken `PAAS_ACTIVATOR_IP` değerini yazdırır;
`PAAS_ACTIVATOR_UPSTREAM=http://127.0.0.1:80` kullanılır.


## Analitik (Faz 19)

Vercel'in Observability / Analytics ekranlarının kendi sunucumuzda çalışan karşılığı: istek
sayıları, hata oranları, yanıt süresi yüzdelikleri, deploy sağlığı ve canlı CPU / bellek kullanımı.
Ek bir izleme yığını (Prometheus, Grafana) gerekmez; veriler mevcut Postgres'te tutulur.

**İstek metrikleri.** Toplayıcı (`internal/analytics`) dakikada bir Traefik'in servis başına
sayaçlarını okur (`traefik_service_requests_total` ve varsa `traefik_service_request_duration_seconds`
histogramı; sıfıra ölçeklemenin kullandığı aynı pod proxy yolu). Sayaçlar kümülatif olduğundan farkı
Traefik pod'u × servis serisi başına hesaplar ve deploy başına bir dakikalık satır olarak
`request_metrics` tablosuna yazar (migration 012; trafik olmayan dakikalar satır üretmez):

- Sayaçlardan biri azalmışsa (Traefik yeniden başladı) seri sıfırlanmış sayılır, güncel değer fark olur.
- Önceki okumadan sonra ortaya çıkan seri (yeni deploy'un ilk isteği, sonradan başlayan Traefik pod'u)
  tamamen sayılır; toplayıcının ilk okuması yalnızca taban çizgisidir (öncesi bilinmez).
- Başarısız okuma hiçbir şey yazmaz; bir sonraki başarılı okuma aradaki tüm istekleri taşır.
- Durum kodları 2xx / 3xx / 4xx / 5xx sınıflarına ayrılır; **hata** = 5xx.
- p50 / p95, Prometheus'un `histogram_quantile` yöntemiyle (kova içinde doğrusal ara değer)
  dakikalık histogramların toplamından hesaplanır.
- `PAAS_ANALYTICS_RETENTION` (varsayılan `168h`) süresinden eski satırlar saatte bir silinir.

**Traefik ayarı.** Servis başına seriler için Traefik'te `metrics.prometheus.addServicesLabels`
açık olmalıdır. Kapalıysa kontrol düzlemi `traefik_service_requests_total not exported (enable
metrics.prometheus.addServicesLabels)` uyarısı verir; sıfıra ölçekleme hiçbir deploy'u uyutmaz ve
analitik boş kalır. [infra/k8s/05-traefik-config.yaml](../infra/k8s/05-traefik-config.yaml) k3s'in
Traefik'i için bir `HelmChartConfig`'tir (`kube-system/traefik`): servis ve entrypoint etiketlerini
açar, p50 / p95 için daha ince histogram kovaları (`0.005 … 10 s`) tanımlar. Bootstrap tüm
manifest'lerle birlikte uygular; çalışan bir kümede:

```bash
kubectl apply -f infra/k8s/05-traefik-config.yaml
kubectl -n kube-system get pods -l app.kubernetes.io/name=traefik   # yeni pod gelir (sayaçlar sıfırlanır)
```

`scripts/k3d-up.sh` aynı dosyayı yerel kümeye de uygular.

**Kaynak kullanımı.** `GET /api/apps/{name}/usage`, uygulamanın çalışan pod'larının CPU (millicore)
ve bellek (working set, bayt) değerlerini k3s ile gelen metrics-server'dan (`metrics.k8s.io`)
anlık okur; değerler saklanmaz. Pod limitleri de döner, böylece kullanım oranı gösterilebilir.
Uyuyan deploy'ların pod'u olmadığından listede yer almazlar. metrics-server yoksa `503` döner.
Kontrol düzleminin `paas-app-manager` rolünde `metrics.k8s.io/pods` (get, list) vardır (Faz 29'dan beri yalnızca uygulama namespace'lerinde).

**Deploy sağlığı.** `GET /api/apps/{name}/health`, hazır her deploy için son 15 dakikadaki istek ve
5xx sayısını, hata oranını ve durumu döner: `no_traffic`, `healthy` (5xx < %1), `degraded`
(< %10), `failing`.

```bash
curl -H "Authorization: Bearer $PAAS_API_TOKEN" "localhost:8080/api/apps/blog/analytics?range=24h"
# {"range":"24h","step_seconds":600,"histogram":true,
#  "totals":{"requests":1130,"status_2xx":1110,…,"errors":15,"error_rate":0.013,"p50_ms":50,"p95_ms":550,"avg_ms":50},
#  "series":[{"time":"…","requests":100,"errors":5,"p50_ms":50,…},…],
#  "deployments":[{"deployment_id":12,"commit_sha":"…","production":true,"requests":120,…}]}
```

| Aralık | Adım | Nokta |
|---|---|---|
| `1h` | 1 dakika | 60 |
| `24h` (varsayılan) | 10 dakika | 144 |
| `7d` | 1 saat | 168 |

`&deployment=<id>` tek bir deploy'a süzer. Gecikme alanları histogram yoksa `null`'dır.

| Değişken | Açıklama |
|---|---|
| `PAAS_ANALYTICS_INTERVAL` | Traefik okuma aralığı (varsayılan `1m`, `0` toplamayı kapatır; `1m` dakika başına hizalanır) |
| `PAAS_ANALYTICS_RETENTION` | dakikalık satırların saklama süresi (varsayılan `168h`) |

Toplayıcı ve kullanım ucu yalnızca `kubernetes` deployer ile çalışır; dry-run'da analitik boş döner,
`/usage` `501` verir.


## Deploy koruması (Faz 24)

Vercel'in "Deployment Protection"ı: bir projenin deploy ve önizleme adresleri (istenirse canlı adres ve
alan adları da) yalnızca izin verilen kişilere açılır. Ayar proje başınadır (Ayarlar → Koruma ya da
`PUT /api/apps/{name}/protection`):

| Mod | Kim açabilir? |
|---|---|
| `off` | Herkes (varsayılan) |
| `team` | Projenin ekibindeki, platforma giriş yapmış herkes (izleyici dahil) |
| `password` | Ortak şifreyi bilen herkes; ekip üyeleri şifresiz girer |

| Kapsam | Korunan adresler |
|---|---|
| `previews` (varsayılan) | `<sha7>-<app>.<domain>` deploy adresleri ve `<branch>-<app>.<domain>` önizlemeleri; canlı adres ve özel alan adları açık kalır |
| `all` | Yukarıdakiler + `<app>.<domain>`, özel alan adları ve kademeli yayının `IngressRoute`'ları |

**Nasıl uygulanır.** Koruma Traefik'te, istek uygulamaya ulaşmadan önce çalışır. Korunan projenin
namespace'inde tek bir `Middleware` (`traefik.io/v1alpha1`, dinamik istemciyle) oluşur:

```
Middleware paas-protect   forwardAuth.address = $PAAS_FORWARD_AUTH_URL?app=<app>
Ingress (kapsamdakiler)   traefik.ingress.kubernetes.io/router.middlewares: app-<app>-paas-protect@kubernetescrd
```

Açıklama (annotation) `applyIngress` içinde hesaplanır; deploy'un Ingress'i ilk yazıldığı anda korumalıdır
ve periyodik uzlaştırma onu bir an bile silmez. Ayar değişince `routing.Syncer` var olan deploy
Ingress'lerini günceller (`ApplyProtection`); koruma kapanınca önce referanslar, sonra `Middleware` silinir.
Traefik CRD'leri yoksa koruma açıkken rota güncellemesi hata verir; adres korumasız kalmaz.

Traefik her isteği `/_auth/verify`'a sorar (kontrol düzlemi, `internal/protect`). İstek şunlardan biriyle geçer:

1. **Erişim çerezi** `_paas_protect`: uygulamanın kendi adresinde (Domain'siz), `HttpOnly`, `SameSite=Lax`,
   HTTPS'te `Secure`; 12 saat. İçeriği `PAAS_SESSION_KEY`'den türeyen anahtarla imzalı: proje, adres (başka
   adreste geçersiz), koruma sürümü (mod ya da şifre değişince herkesin oturumu kapanır) ve erişim türü (ekip
   üyeliği her istekte doğrulanır, ekipten çıkan kişinin erişimi biter; paylaşım bağlantısı iptal edilince
   erişimi biter).
2. **Paylaşım bağlantısı**: `?paas_share=<token>` ile açılan adres çerezi kurar ve token'sız adrese yönlendirir.
   Token'ın yalnızca SHA-256'sı saklanır, isteğe bağlı süre (1 gün – 1 yıl ya da süresiz), istendiğinde iptal.
3. **Otomasyon anahtarı**: `x-paas-protection-bypass: <anahtar>` başlığı (ör. CI'daki uçtan uca testler).
   Yalnızca SHA-256'sı saklanır; yenilenince eskisi hemen geçersiz olur.

Hiçbiri yoksa GET istekleri `PAAS_PUBLIC_URL/_protect/login?app=<app>&next=<adres>` sayfasına gider (diğer
yöntemler `401`). Ekip modunda platform oturumu kullanılır (yoksa önce GitHub girişi); şifre modunda Türkçe
şifre formu çıkar. Başarılı girişte 2 dakikalık, **tek kullanımlık** imzalı bir token uygulamanın adresine
`/_paas/auth?token=…` ile gönderilir; verify ucu bu yolu kendisi karşılar (ayrı Ingress kuralı gerekmez),
çerezi kurar ve asıl sayfaya döner. `next` yalnızca projenin kendi adreslerinden biri olabilir (alias, alan
adı ya da veritabanındaki bir deploy); başka adrese yönlendirme yoktur. Şifreler PBKDF2-HMAC-SHA256 ile
(standart kütüphane, 600.000 tur, rastgele tuz) saklanır; aynı istemciden 15 dakikada 10 yanlış şifreden sonra
deneme durur.

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `PAAS_FORWARD_AUTH_URL` | `http://paas.paas.svc.cluster.local/_auth/verify` | Traefik'in kontrol düzlemine ulaştığı adres. Kontrol düzlemi k3d'nin dışında, makinede çalışıyorsa: `http://<host.docker.internal IP'si>:<port>/_auth/verify` |
| `PAAS_PUBLIC_URL` | `https://<domain>` | Giriş sayfasının adresi |
| `PAAS_SESSION_KEY` | — | Çerez ve token imzalarının anahtarı (yoksa API token'ından türetilir) |

TLS'siz k3d'de (`PAAS_INGRESS_TLS=false`) çerez `Secure` olmadan kurulur ve yönlendirmeler `http://` kalır;
TLS'te `X-Forwarded-Proto: https` ile `Secure` olur. Notlar: korunan projeler `/_paas/auth` yolunu kendileri
sunamaz; ayar, üyelik ve paylaşım bağlantısı okumaları 5 saniye önbelleklenir, bir değişiklik en geç bu sürede
geçerli olur.


## Preview yorumları (Faz 25)

Vercel Comments'in karşılığı: ekip üyeleri hazır bir deploy'u kontrol panelinde açar, sayfanın bir noktasına
yorum bırakır, yanıtlaşır ve çözülünce kapatır. Yorumlar deploy'a (ve uygulamaya) bağlıdır; deploy bir PR'ın
branch'inden geldiyse her yorum dizisi PR'a da yorum olarak yazılır. Tablo: `preview_comments` (migration 018).

**İnceleme sayfası** — `/deployments/{id}/review`. Deploy listesinde ve proje sayfasında her hazır deploy'un
yanında **İncele** bağlantısı ve açık yorum sayısı rozeti, deploy sayfasında da **İncele** düğmesi var.

- Deploy, `sandbox="allow-scripts allow-same-origin allow-forms"` ile bir iframe'de açılır. Deploy başka bir
  origin'de (`<sha7>-<app>.<domain>`) çalıştığı için sayfa iframe'in DOM'una, kaydırma konumuna ya da adresine
  erişemez; `allow-same-origin` yalnızca uygulamanın kendi çerezleri ve depolaması için verilir. Deploy adresi
  kontrol paneliyle aynı olsaydı sandbox kaldırılabilirdi; bu durumda iframe hiç gösterilmez.
- CSP yalnızca bu sayfada `frame-src <scheme>://*.<domain>` ile genişler; diğer sayfalar `default-src 'self'`
  ile kalır, `frame-ancestors 'none'` her yerde geçerlidir.
- **Sayfa** alanına yorumlanacak yol yazılır (`/fiyatlar`), "Git" ile iframe o sayfaya gider. **Tam / 1280 /
  768 / 375** iframe genişliğini seçer.
- **Yorum modu** açıkken iframe'in üstündeki saydam katmana tıklamak bir nokta bırakır: x / y görünen alanın
  yüzdesi olarak, yol ve iframe genişliğiyle birlikte kaydedilir. Nokta seçmeden yazılan yorum sabitlenmemiş
  genel bir yorumdur. Noktalar numaralı iğnelerle gösterilir; yan paneldeki yoruma tıklamak sayfasını ve
  genişliğini geri yükler.
- Yan panel: açık diziler, yanıtlar, "Çözüldü" / "Yeniden aç", "Sil"; çözülenler ayrı bir bölümde.
  Formlar htmx ile yalnızca paneli yeniler (iframe yeniden yüklenmez); JavaScript olmadan da çalışır.
  Her form CSRF ve aynı-origin kontrolünden geçer.
- `@kullanıcı` ile ekip üyeleri anılır: yalnızca ekipte olan GitHub kullanıcı adları geçerli sayılır,
  kanonik yazımıyla saklanır (`mentions`) ve vurgulanır. Paneldeki üye adlarına tıklamak onları ekler.

**Yetki** (deploy'un uygulamasının ekibi üzerinden):

| İşlem | Gereken |
|---|---|
| Okumak, yorum ve yanıt yazmak | viewer (inceleyenler) |
| Diziyi çözmek / yeniden açmak | member, ya da dizinin yazarı |
| Silmek | yorumun yazarı; ekip sahibi (owner) her yorumu kaldırabilir |

Diğer ekiplerin yorumları `404` döner. Yorum düz metindir: en fazla 2000 karakter, kontrol karakteri yok;
yol `/` ile başlar (`//` olmaz), en fazla 1024 karakter, boşluksuz; x / y 0–100 ve ikisi birlikte. Yeni dizi
yalnızca hazır deploy'a açılır (`409`).

```bash
# Noktaya sabitlenmiş yorum
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/api/deployments/12/comments \
  -d '{"body":"Başlık mobilde taşıyor @alice","path":"/fiyatlar","x":42.5,"y":18,"viewport_width":375}'
# Yanıt, çözme, yeniden açma, silme
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/api/comments/5/replies -d '{"body":"Düzeltildi"}'
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/api/comments/5/resolve
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/api/comments/5/reopen
curl -X DELETE -H "Authorization: Bearer $TOKEN" localhost:8080/api/comments/5
# Deploy'un ya da uygulamanın dizileri ve sayıları
curl -H "Authorization: Bearer $TOKEN" "localhost:8080/api/deployments/12/comments?status=open"
curl -H "Authorization: Bearer $TOKEN" "localhost:8080/api/apps/blog/comments?status=open&limit=50"
# {"counts":{"open":3,"resolved":1},"threads":[{"id":5,"path":"/fiyatlar","x":42.5,…,"replies":[…]}]}
```

**GitHub senkronu.** Commit status'ları açıksa (`PAAS_GITHUB_STATUS` `false` değilse ve GitHub App ya da
`PAAS_GITHUB_TOKEN` varsa) her dizi değişikliği bir kuyruğa girer, tek bir arka plan döngüsü sırayla işler:
deploy'un branch'indeki açık PR'lar bulunur (Faz 5'teki PR yorumu mantığı), dizi her PR'a gizli bir işaretle
(`<!-- paas:comment:<id> -->`) tek yorum olarak yazılır; yanıt, çözme ve yeniden açmada aynı yorum güncellenir,
dizi silinince PR yorumu da silinir. PR yorumunda durum (🟢 Açık / ✅ Çözüldü), yazar, yol ve genişlik, commit,
son 20 yanıt ve inceleme sayfasına bağlantı (`PAAS_PUBLIC_URL/deployments/<id>/review?thread=<id>`) bulunur.
Kimlik doğrulama Faz 15'teki kurulum token'larıyla yapılır. İstek GitHub'ı beklemez; hatalar loglanır ve yorumu
yazana yansımaz. Kullanıcı metni HTML'siz alıntılanır (sahte işaret yazılamaz); yalnızca geçerli anmalar
GitHub'da `@` olarak kalır, diğer `@kelime`ler ve yazar adları bildirim göndermez.


## Bildirimler (Faz 26)

Her ekip, projelerinde olan biteni kendi kanallarına alır. Ekip sayfasındaki **Bildirimler** düğmesi
(`/teams/<ekip>/notifications`) kanalları, son gönderimlerini, **Test gönder** düğmesini ve kendi e-posta
tercihlerini gösterir. viewer okur; member kanal ekler, değiştirir, siler ve test mesajı gönderir.

**Kanal türleri**

| Tür | Nereye | Biçim |
|---|---|---|
| `email` | Elle yazılan adresler (en fazla 20) ve / veya e-posta adresini tercihlerine kaydetmiş ekip üyeleri | Türkçe düz metin (UTF-8, quoted-printable), `Message-ID` teslimat anahtarından türer |
| `slack` | Slack incoming webhook URL'si | Blok mesajı, durum rengine boyanmış kenar çubuğu, "Deploy'u aç" düğmesi |
| `discord` | Discord webhook URL'si | Renkli embed, alanlar; `@everyone` gibi anmalar kapalı |
| `webhook` | Herhangi bir HTTPS adresi | İmzalı JSON (`X-Paas-Signature`) |

Kanal tüm projeleri ya da seçilen projeleri izler ve olayların bir alt kümesine abone olur. Webhook URL'leri ve imza
anahtarları env anahtarlığıyla (Faz 10, AES-256-GCM, kanal kimliğine bağlı) şifreli saklanır, `paas-envkey` rotasyonu
onları da yeniden şifreler; API ve arayüz URL'nin yalnızca `https://host/…` kısmını gösterir. Webhook kanalının imza
anahtarı (`whsec_…`) yalnızca oluşturulurken (ya da "İmza anahtarını yenile" ile) bir kez gösterilir.

**Olaylar**

| Olay | Ne zaman | Varsayılan |
|---|---|---|
| `deployment.ready` | Deploy hazır | kapalı (yoğun ekiplerde çok sık) |
| `deployment.failed` | Deploy başarısız (hatanın ilk satırıyla) | açık |
| `deployment.canceled` | Deploy iptal edildi (Faz 17) | açık |
| `production.changed` | Rollback, promote, canary'nin tamamlanması, elle sonuçlandırılan yayın | açık |
| `rollout.rolled_back` | Kademeli yayın metrikler bozulduğu için kendiliğinden geri alındı ya da durdu (Faz 21) | açık |
| `addon.failed` | Veritabanı hazırlanamadı, branch kopyası / yedek / geri yükleme başarısız (Faz 22) | açık |
| `domain.failed` | Yönlendirilen alan adının DNS'i bozuldu ya da yeni alan adı bir gün sonra hâlâ doğrulanmadı (Faz 12) | açık |
| `domain.certificate_expiring` | Sertifikanın bitmesine 14 günden az kaldı ve cert-manager yenileyemedi | açık |
| `limit.reached` | Bir limite ulaşıldı (`notify.Limit` + `Emitter.Emit`; plan ve limitler bunu kullanacak) | açık |

Üreticiler yalnızca `notify.Emitter` arayüzünü görür (`Emit(ctx, Event)`); `cmd/paas` tek bir `notify.Service`'i
worker'a, rollout denetleyicisine, eklenti denetleyicisine, alan adı doğrulayıcısına, API'ye ve arayüze verir.
Deploy olayları worker'ın mevcut bildirim yolundan gelir: `notify.Chain` olay üreticisini GitHub bildiricisinin yanına
zincirler, worker'daki çağrı noktaları değişmez.

**Teslimat.** `Emit` olayı `(ekip, tür, anahtar)` başına bir kez saklar ve eşleşen her kanal için outbox'a bir satır
yazar (aynı olayı iki kopya ya da yeniden denenen bir adım üretse de tek mesaj gider). Arka plandaki gönderici satırları
`FOR UPDATE SKIP LOCKED` ile kiralar, Postgres `LISTEN/NOTIFY` ile hemen uyanır, başarısız denemeleri üstel bekleme
(30 sn, 1 dk, 2 dk … en fazla 1 sa; 429'da `Retry-After`) ile tekrarlar ve `PAAS_NOTIFY_MAX_ATTEMPTS` denemeden sonra
vazgeçer. 4xx yanıtlar ve 5xx SMTP yanıtları kalıcı hatadır. Kiralaması dolan (gönderirken çöken) satır yeniden alınır:
teslimat en az bir kez yapılır, alıcılar `X-Paas-Delivery` / `Idempotency-Key` ile tekrarı ayıklar. Kanal başına dakikada
`PAAS_NOTIFY_RATE_LIMIT` mesaj gönderilir, fazlası sırasını bekler. Webhook istekleri yönlendirme izlemez ve DNS
çözümlemesinden sonra özel / yerel adreslere (küme içi, bulut metadata servisi) bağlanmaz. Gönderim günlüğü 30 gün tutulur.

**E-posta tercihleri.** Her kullanıcı adresini, e-postayı tümden kapatmayı ve almak istemediği olayları seçer; tercihler
adres bir kanalda elle yazılmış olsa da uygulanır. Test mesajları tercihlere bakmaz. SMTP bağlantısı STARTTLS olmadan
e-posta göndermez (465 için `PAAS_SMTP_SECURITY=tls`).

**Webhook imzası.** Gövde `WebhookBody` JSON'udur (`id`, `type`, `created_at`, `team`, `app`, `deployment_id`, `severity`,
`title`, `text`, `url`, `fields`, `data`). Alıcı şunu doğrular:

```
X-Paas-Timestamp: 1791561600
X-Paas-Signature: sha256=hex(HMAC-SHA256(whsec_…, "<X-Paas-Timestamp>.<gövde>"))
X-Paas-Event: deployment.failed
X-Paas-Delivery: <teslimat anahtarı>
```

Karşılaştırma sabit sürede yapılmalı, eski zaman damgaları reddedilmeli (`notify.VerifySignature` örnek uygulamadır).

**Ayarlar**

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `PAAS_SMTP_HOST`, `PAAS_SMTP_PORT` | — , 587 | SMTP sunucusu; boşsa e-posta kanalı açılamaz |
| `PAAS_SMTP_USER`, `PAAS_SMTP_PASSWORD` | — | AUTH PLAIN (yalnızca TLS üstünde) |
| `PAAS_SMTP_FROM` | — | Gönderen adresi (`PAAS_SMTP_HOST` ile zorunlu) |
| `PAAS_SMTP_SECURITY` | `starttls` | `starttls`, `tls` (465) ya da `none` (yalnızca AUTH'suz geliştirme sunucusu) |
| `PAAS_PUBLIC_URL` | — | Mesajlardaki deploy ve ayar bağlantılarının kökü |
| `PAAS_NOTIFY_INTERVAL` | `5s` | `LISTEN` dışında outbox'a bakma aralığı |
| `PAAS_NOTIFY_MAX_ATTEMPTS` | `8` | Teslimat başına deneme (1–20) |
| `PAAS_NOTIFY_RATE_LIMIT` | `30` | Kanal başına dakikada mesaj (0 sınırsız) |
| `PAAS_NOTIFY_ALLOW_PRIVATE` | `false` | `true`: `http://` ve özel adreslere izin (yalnızca geliştirme) |

**API**

```bash
# Kanallar (viewer okur, member değiştirir)
curl -H "Authorization: Bearer $TOKEN" $PAAS/api/teams/web/notifications
curl -X POST -H "Authorization: Bearer $TOKEN" $PAAS/api/teams/web/notifications \
  -d '{"name":"#deploys","kind":"slack","url":"https://hooks.slack.com/services/…","events":["deployment.failed","production.changed"],"apps":["blog"]}'
curl -X POST -H "Authorization: Bearer $TOKEN" $PAAS/api/teams/web/notifications \
  -d '{"name":"ci","kind":"webhook","url":"https://ci.example.com/paas"}'
# {"id":2,…,"signing_secret":"whsec_…"}   (bir kez gösterilir)
curl -X PUT  … /api/teams/web/notifications/2 -d '{"enabled":false}'          # boş url saklananı korur
curl -X PUT  … /api/teams/web/notifications/2 -d '{"rotate_secret":true}'
curl -X POST … /api/teams/web/notifications/2/test                             # 200 gönderildi, 502 hata, 202 kuyrukta
curl         … /api/teams/web/notifications/2/deliveries?limit=50
curl -X DELETE … /api/teams/web/notifications/2
# Olay türleri ve kendi e-posta tercihlerin
curl … /api/notifications/events
curl -X PUT … /api/me/notifications -d '{"email":"ben@example.com","email_enabled":true,"muted_events":["deployment.ready"]}'
```


## Tarayıcıdan terminal (Faz 27)

Çalışan bir deploy'un pod'unda, tarayıcıdan gerçek bir kabuk: `kubectl exec -it` kurulumu, kubeconfig ya da küme erişimi
gerekmez. Deploy listesinde ve deploy sayfasında hazır deploy'ların yanında **Terminal** düğmesi (yalnızca member ve owner) →
`/apps/{app}/deployments/{id}/terminal`.

- **Nasıl çalışır:** sayfa [xterm.js](https://xtermjs.org)'i unpkg'den sabit sürüm ve SRI bütünlük özetiyle yükler
  (`@xterm/xterm@5.5.0`, `@xterm/addon-fit@0.10.0`); CSP yalnızca bu sayfada `style-src`'ye unpkg'i ve `connect-src`'ye
  aynı hostun `ws://` / `wss://` adresini ekler. Tarayıcı aynı adrese WebSocket açar (`Upgrade: websocket`); sunucu tarafı
  standart kütüphaneyle yazılmış RFC 6455 uygulamasıdır (`internal/wsock`: el sıkışma, parçalı ve maskeli çerçeveler,
  ping / pong, kapanış kodları). Kontrol düzlemi pod'a client-go'nun `remotecommand`'ı ile TTY'li `exec` açar (önce WebSocket,
  desteklemeyen API sunucularında SPDY), tuş vuruşlarını ve pencere boyutunu iletir. Kabuk `/bin/sh`'dir; imajda `bash` varsa o açılır.
- **Süreç / pod seçimi:** varsayılan, web sürecinin en yeni hazır pod'udur. Birden çok pod varsa (birden çok kopya, Faz 20
  worker'ları, çalışan bir cron) sayfadaki seçiciden biri açılır; yalnızca o deploy'un pod'ları seçilebilir. Web'siz bir
  uygulamada ilk worker açılır.
- **Kabuksuz imajlar:** paas'ın Go imajları gibi distroless / scratch imajlarda `/bin/sh` yoktur; oturum sessizce düşmez,
  "Bu imajda kabuk yok" mesajı gösterilir (kapanış kodu 4004).
- **Güvenlik:**
  - Yalnızca tarayıcı oturumu (bearer token'la açılmaz), ekipte member+ rolü (viewer 403, başka ekip 404).
  - WebSocket'in `Origin`'i bu host olmalı (yoksa 403: siteler arası WebSocket ele geçirmeye karşı); CSRF token'ı ilk mesajda
    (`{"type":"auth","csrf":"…"}`, URL'de ve loglarda görünmez) ya da `?csrf=` ile gelir. Token doğrulanmadan pod'a tek bayt gitmez.
  - **10 dakika** işlem yapılmazsa (tuş ya da pencere boyutu) ve her durumda **60 dakika** sonra oturum kapanır; kullanıcı başına
    aynı anda en fazla **3** oturum.
  - Emekliye ayrılmış, henüz hazır olmayan ve uyuyan deploy'larda terminal açılmaz (WebSocket 409). Uyuyan deploy'un sayfası
    "Uyandır ve terminali aç" düğmesini gösterir (Faz 11 ölçekleyicisiyle uyandırır; açık bir oturum deploy'u uyanık tutar);
    sıfıra ölçekleme kapalıysa siteyi açınca uyanacağını söyler.
  - Her oturum denetim kaydına yazılır (`audit_log`, migration 020): kim, uygulama, deploy, süreç, pod, başlangıç, bitiş, çıkış kodu
    ve kapanma nedeni (`exit`, `idle`, `max_duration`, `client_closed`, `no_shell`, `error`, `shutdown`). **Tuş vuruşları ve çıktı
    kaydedilmez.** Kayıtlar projenin Genel sekmesinde "Terminal oturumları" altında üyelere listelenir.
  - Kümede `pods/exec` (`get`, `create`) yetkisi gerekir (`paas-app-manager`, `infra/k8s/30-control-plane-rbac.yaml`).
- **Ayarlar:**

| Değişken | Anlamı |
|---|---|
| `PAAS_TERMINAL` | `false` terminali tamamen kapatır: rotalar 404, düğmeler görünmez (varsayılan açık; Kubernetes deployer'ı gerekir) |
| `PAAS_TERMINAL_IDLE_TIMEOUT` | girdi olmadan kapanma süresi (varsayılan `10m`) |
| `PAAS_TERMINAL_MAX_DURATION` | azami oturum süresi (varsayılan `60m`) |
| `PAAS_TERMINAL_MAX_SESSIONS` | kullanıcı başına eşzamanlı oturum (varsayılan `3`) |

- **Protokol** (tarayıcı ↔ kontrol düzlemi, tek WebSocket): ikili çerçeveler tuş vuruşları / terminal çıktısı; metin çerçeveleri
  JSON: tarayıcıdan `auth`, `resize` (`cols`, `rows`); sunucudan `ready` (pod, süreç, süre sınırları), `exit` (`code`),
  `closed` ve `error` (Türkçe `message`). Kapanış kodları: 1000 kabuk çıktı, 4000 işlem yok, 4001 azami süre, 4003 doğrulama,
  4004 kabuk yok, 4009 çalışan pod yok, 4029 oturum sınırı, 1001 sunucu kapanıyor.
- **Bağlantı koparsa** sayfadaki "Yeniden bağlan" yeni bir oturum açar (eski kabuk kapanmıştır; kayıt ayrı satırdır).


## Kullanıcılar ve ekipler (Faz 13)

Uygulamalar bir **ekibe** aittir; kullanıcılar GitHub hesaplarıyla giriş yapar ve her ekipte bir rolleri
vardır. Bir ekip yalnızca kendi uygulamalarını görür: başka ekibin uygulaması veya deployment'ı,
hiç yokmuş gibi **404** döner (isimler sızmaz). Migration 007 mevcut tüm uygulamaları `default`
ekibine taşır.

| Rol | Yetki |
|---|---|
| `viewer` | okuma: uygulamalar, deployment'lar, build ve pod logları, SSE akışları, env anahtarları, ekip üyeleri |
| `member` | + deploy'u etkileyen işlemler: rollback, ortam değişkenleri, ekipte uygulama oluşturma |
| `owner` | + ekip yönetimi: üye ekleme/çıkarma, rol değiştirme (son owner düşürülemez) |

Webhook ucu değişmedi (HMAC imzası). Yetersiz rol **403**, ekip dışı **404**, kimliksiz istek **401** alır.

**GitHub OAuth App oluşturma**

1. github.com → *Settings* → *Developer settings* → *OAuth Apps* → *New OAuth App*
   (bir organizasyon adına: organizasyon *Settings* → *Developer settings* → *OAuth Apps*).
2. *Homepage URL*: `PAAS_PUBLIC_URL` (ör. `https://paas.example.com`).
3. *Authorization callback URL*: `<PAAS_PUBLIC_URL>/auth/github/callback`
   (yerelde ör. `http://localhost:8080/auth/github/callback`, o zaman `PAAS_PUBLIC_URL=http://localhost:8080`).
4. *Register application* → *Client ID*'yi `PAAS_GITHUB_OAUTH_CLIENT_ID`'ye yazın, *Generate a new client
   secret* ile üretilen sırrı `PAAS_GITHUB_OAUTH_CLIENT_SECRET`'e yazın.

Akış: yetkilendirme kodu + PKCE (S256); `state`, PKCE doğrulayıcısı ve dönüş yolu 10 dakikalık, HMAC
imzalı bir çerezde (`SameSite=Lax`, yalnızca `/auth/github` yolunda) tutulur, callback bu çerezle eşleşmeyen
isteği reddeder. GitHub erişim token'ı yalnızca girişte `/user` (ve gerekirse `/user/orgs`) için kullanılır,
saklanmaz. İzin kapsamı boştur (herkese açık profil); organizasyon kontrolü açıksa `read:org` istenir.

**Kim giriş yapabilir** (hiçbiri ayarlanmamışsa yalnızca bir ekibe eklenmiş kullanıcılar; rastgele bir
GitHub hesabı asla giremez):

| Değişken | Anlamı |
|---|---|
| `PAAS_ADMIN_GITHUB_LOGINS` | Her zaman girebilir ve her girişte `default` ekibinin **owner**'ı yapılır (ilk kurulum) |
| `PAAS_ALLOWED_GITHUB_USERS` | Virgülle ayrılmış GitHub kullanıcı adları |
| `PAAS_ALLOWED_GITHUB_ORG` | Bu organizasyon(lar)ın üyeleri (`/user/orgs` ile kontrol, `read:org` kapsamı) |
| (davet) | Bir ekip owner'ının eklediği kullanıcılar, listelerde olmasalar da girebilir |

**İlk kurulum (bootstrap):** "ilk giren owner olur" yerine `PAAS_ADMIN_GITHUB_LOGINS` seçildi: ilk girişi
kimin yapacağı bir yarışa bırakılmaz ve kural yapılandırmadan okunabilir. Admin olmadan da kurulum
mümkündür: eski API token'ı ile `PUT /api/teams/default/members/<login>` `{"role":"owner"}`.

**Oturumlar:** imzalı, durumsuz çerez artık kullanıcı kimliği ve bitiş zamanı taşır
(`<bitiş>.<kullanıcı id>.<nonce>.<HMAC>`). Anahtar `PAAS_SESSION_KEY` (en az 32 karakter), yoksa
`PAAS_API_TOKEN`'dan türetilir. Silinmiş kullanıcının oturumu geçersizdir; her istekte kullanıcı ve rolü
veritabanından okunur, rol değişikliği anında etkilidir.

**Kişisel API token'ları:** arayüzde *Tokens* sayfası veya `POST /api/tokens`. Biçim `paas_` + 256 bit
rastgele değer (base64url); yalnızca **bir kez** gösterilir, veritabanında SHA-256 özeti saklanır (değer
rastgele olduğu için yavaş bir hash gerekmez). Listede ad, ilk karakterler (`paas_ab12cd…`), oluşturma,
son kullanım (dakikada en fazla bir yazma) ve bitiş zamanı görünür. İsteğe bağlı süre (1–3650 gün);
süresi dolmuş veya iptal edilmiş token **401** alır. Token sahibinin rollerini taşır.

**Eski `PAAS_API_TOKEN`:** admin / acil durum (break-glass) token'ı olarak çalışmaya devam eder: tüm
ekiplerde owner yetkisi, kullanıcısı olmadığı için kişisel token'ı yoktur; uygulama oluştururken ekip
verilmezse `default` ekibi kullanılır. GitHub girişi yapılandırılmışsa **boş bırakılabilir** (o zaman
devre dışıdır; oturum anahtarı için `PAAS_SESSION_KEY` zorunlu olur). GitHub girişi yoksa (geliştirme
modu) zorunludur ve arayüzdeki token giriş formu admin oturumu açar; GitHub girişi açıkken bu form
kapalıdır ve admin oturum çerezleri reddedilir.

**Yeni uçları korumak:** uygulama başına her uç tek bir sarmalayıcıyla korunur
(`internal/api/access.go`):

```go
api.HandleFunc("GET /api/apps/{name}/domains", s.requireApp(store.RoleViewer, s.listDomains))
api.HandleFunc("POST /api/apps/{name}/domains", s.requireApp(store.RoleMember, s.addDomain))
```

İşleyici `s.lookupApp(w, r)` ile önceden kontrol edilmiş uygulamayı alır. Sarmalanmamış bir işleyici
`lookupApp` çağırırsa yine korunur (GET/HEAD için viewer, diğerleri için member). Web arayüzünde
`loadApp` aynı kuralı uygular.


## GitHub entegrasyonu (Faz 5)

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


## GitHub App (Faz 15)

Kişisel token, ayrı OAuth App ve repo başına webhook yerine **tek bir GitHub App**. Kullanıcı arayüzde
*Import from GitHub* → *Install GitHub App* der, GitHub'da repoları seçer, platforma döner; seçtiği repolar
listede görünür ve tek tıkla uygulama oluşur, production branch'inin son commit'i ilk deploy olarak
kuyruğa girer. Yeni bir proje eklemek GitHub'da hiçbir ayar gerektirmez: webhook App'e aittir, push'lar
ve PR'lar kurulumun kapsadığı her repodan gelir.

**1. App'i oluşturma** (github.com → sağ üstteki profil resmi → *Settings* → en altta *Developer settings* →
*GitHub Apps* → *New GitHub App*; organizasyon adına: organizasyon sayfası → *Settings* → *Developer settings*
→ *GitHub Apps* → *New GitHub App*):

| Alan | Değer |
|---|---|
| *GitHub App name* | ör. `paas-ornek`; URL adı (*slug*) buradan türetilir: `github.com/apps/paas-ornek` |
| *Homepage URL* | `PAAS_PUBLIC_URL` (ör. `https://paas.example.com`) |
| *Callback URL* | `<PAAS_PUBLIC_URL>/auth/github/callback` (giriş ve kurulum doğrulaması aynı adresi kullanır) |
| *Expire user authorization tokens* | açık kalabilir (kullanıcı token'ı saklanmaz, yalnızca o istekte kullanılır) |
| *Request user authorization (OAuth) during installation* | **kapalı**: açıkken GitHub *Setup URL*'i devre dışı bırakır; doğrulama aşağıdaki PKCE'li akışla yapılır |
| *Enable Device Flow* | kapalı |
| *Setup URL* | `<PAAS_PUBLIC_URL>/github/setup` |
| *Redirect on update* | **açık** (repo seçimi değişince kullanıcı import sayfasına döner) |
| *Webhook* → *Active* | açık |
| *Webhook URL* | `<PAAS_PUBLIC_URL>/webhooks/github` |
| *Webhook secret* | `PAAS_GITHUB_WEBHOOK_SECRET` ile aynı değer |

*Permissions* → *Repository permissions*:

| İzin | Seviye | Neden |
|---|---|---|
| *Contents* | Read-only | commit SHA'sıyla clone, branch head'i, varsayılan branch |
| *Commit statuses* | Read and write | `paas/deploy` durumu |
| *Pull requests* | Read and write | PR'daki "Preview hazır" yorumu |
| *Metadata* | Read-only | zorunlu (GitHub kendisi seçer) |

*Organization permissions* → *Members*: Read-only, **yalnızca** `PAAS_ALLOWED_GITHUB_ORG` kullanılıyorsa
(girişte organizasyon üyeliği kontrolü). *Account permissions*: hiçbiri.

*Subscribe to events*: **Push** ve **Pull request**. `installation` ve `installation_repositories` olayları
App'lere her zaman gönderilir; bunlar için işaretlenecek kutu yoktur.

*Where can this GitHub App be installed?*: **Only on this account**: App yalnızca onu oluşturan kullanıcı
veya organizasyona kurulabilir (platform tek bir organizasyonun repolarını deploy ediyorsa doğru seçim).
**Any account**: başka kullanıcılar ve organizasyonlar da kurabilir (platformu farklı GitHub hesaplarına
açıyorsanız). İki durumda da kimin giriş yapabileceğini Faz 13'ün izin listeleri belirler; bir kurulumun
repoları yalnızca onu bağlayan ekibe listelenir.
Platformu herkese açarken (`PAAS_OPEN_SIGNUP=true`, [Faz 23](#planlar-ve-limitler-faz-23)) bu ayar **Any
account** olmalıdır.

*Create GitHub App* → açılan *General* sayfasında:

| GitHub'daki değer | Ortam değişkeni |
|---|---|
| *App ID* | `PAAS_GITHUB_APP_ID` |
| *Private keys* → *Generate a private key* (inen `.pem` dosyası) | `PAAS_GITHUB_APP_PRIVATE_KEY_FILE` (dosya yolu) veya `PAAS_GITHUB_APP_PRIVATE_KEY` (PEM içeriği) |
| URL'deki ad (`github.com/apps/<slug>`) | `PAAS_GITHUB_APP_SLUG` |
| *Client ID* | `PAAS_GITHUB_OAUTH_CLIENT_ID` |
| *Client secrets* → *Generate a new client secret* | `PAAS_GITHUB_OAUTH_CLIENT_SECRET` |

App'in Client ID / secret'ı **ayrı OAuth App'in yerini alır**: "Sign in with GitHub" aynı App üzerinden
çalışır (aynı callback, aynı PKCE akışı); Faz 13'teki OAuth App silinebilir. Private key yalnızca kontrol
düzleminde durur; clone, commit status ve PR yorumu için ondan kısa ömürlü kurulum token'ları üretilir.

**2. Kurulumun ekibe bağlanması** (güvenlik açısından kritik adım): bir kurulumun repoları yalnızca o
kurulumu bağlayan (*claim*) ekibe import için sunulur. Kurulum numarasını bilmek hiçbir şey kanıtlamaz
(numaralar ardışıktır ve URL'lerde görünür), bu yüzden bağlama kullanıcının kendi GitHub hesabıyla
doğrulanır:

```
/import → /github/install?team=<slug>      member+; {ekip, kullanıcı} 30 dk geçerli, HMAC imzalı state'e
        → github.com/apps/<slug>/installations/new?state=…      kullanıcı repoları seçer
        → /github/setup?installation_id=…&setup_action=install|update&state=…
              state doğrulanır; OAuth state + PKCE doğrulayıcı + kurulum + ekip + kullanıcı
              10 dk'lık imzalı çereze (paas_ghclaim, SameSite=Lax, yalnızca /auth/github yolu)
        → github.com/login/oauth/authorize (PKCE S256; App'le daha önce giriş yapıldıysa onay sorulmaz)
        → /auth/github/callback   kod → taze kullanıcı token'ı, ardından:
              1. kullanıcı ekipte hâlâ member+ mı?
              2. GET /user: token, state'teki platform kullanıcısının GitHub hesabına mı ait?
              3. GET /user/installations bu kurulumu listeliyor mu?
              4. satır yoksa (installation webhook'u henüz gelmedi) GitHub'ın yanıtından oluşturulur;
                 kullanıcının görebildiği repolar eklenir (GET /user/installations/{id}/repositories)
              5. ClaimInstallation → /import?ok=github
```

- GitHub'dan dönüş siteler arası bir yönlendirmedir; `SameSite=Lax` oturum çerezi bu üst düzey GET ile gelir ama
  akış ona dayanmaz: kimin başlattığını imzalı state, GitHub'da kimin onayladığını token söyler; ikisi aynı hesap olmalıdır.
  Başkasının kurulum numarasıyla gelen saldırganın token'ı o kurulumu listelemez → **403**, hiçbir şey
  kaydedilmez. Token yalnızca bu çağrılar için kullanılır, saklanmaz.
- Başka bir ekibe bağlı bir kurulumu taşımak o ekipte de member+ olmayı gerektirir (aksi halde **409**).
- State'siz bir *Setup URL* ziyareti (GitHub'daki *Configure* sayfasından *Redirect on update*) hiçbir şeyi
  değiştirmez, import sayfasına döner; repo değişiklikleri `installation_repositories` webhook'uyla gelir.
  Platform dışından kurulmuş, henüz bağlanmamış bir kurulum için import sayfasında *Install GitHub App*'e
  tıklayın: GitHub mevcut kurulumu gösterir ve kaydedince state ile geri gönderir.
- GitHub girişi kapalıysa (geliştirme modu) `/github/setup` bu doğrulamayı yapamaz ve nedenini açıklar.

**3. Import:** `/import` sayfası (uygulamalar listesindeki *Import from GitHub* düğmesi) ekiplerin
kurulumlarındaki repoları hesap adına göre gruplar, zaten import edilmiş repolarda uygulamaya bağlantı
gösterir. Uygulama adı repo adından türetilir (`My_Site.v2` → `my-site-v2`; rakamla başlıyorsa `app-`
öneki alır); production branch'i boş bırakılırsa repo'nun varsayılan branch'idir; ikisi de formda
değiştirilebilir. Birden fazla ekipte member olan kullanıcı kurulum düğmesinin yanında ekibi seçer.
Viewer'lar listeyi görür ama import edemez. API karşılığı:

```bash
curl -s -H "$H" localhost:8080/api/github/repos
curl -s -H "$H" -d '{"repo":"acme/web"}' localhost:8080/api/apps/import
# {"app":{…,"name":"web"},"deployment":{"id":12,"commit_sha":"…","status":"queued",…}}
```

İlk deploy, push webhook'unun kullandığı kuyruk kaydıyla (`EnqueueDeployment`) oluşur; production alias'ı
her zamanki kurallarla verilir. Branch head'i okunamazsa uygulama yine oluşur, yanıtta `warning` döner ve
ilk push deploy eder.

**Eski yol:** `PAAS_GITHUB_TOKEN` (PAT) + repo başına el ile webhook, geliştirme ve App'siz kurulumlar için
çalışmaya devam eder (bkz. [Gerçek GitHub'a bağlamak](#gerçek-githuba-bağlamak)). App yapılandırılmamışsa
import sayfası bu yolu anlatır; arayüzdeki *New app* formu her iki durumda da kullanılabilir.


## Gerçek GitHub'a bağlamak

GitHub App ile repo başına ayar gerekmez: bkz. [GitHub App (Faz 15)](#github-app-faz-15). App'siz yol:

Repo → Settings → Webhooks → Add webhook:
- **Payload URL:** `https://<sunucun>/webhooks/github` (yerelde test için ngrok veya Cloudflare Tunnel)
- **Content type:** `application/json`
- **Secret:** `PAAS_GITHUB_WEBHOOK_SECRET` ile aynı
- **Events:** "Let me select individual events" → **Pushes** ve **Pull requests**
  (PR kapanınca preview temizliği için)


## Planlar ve limitler (Faz 23)

Platformu herkese açmanın (`PAAS_OPEN_SIGNUP=true`) önkoşulu: her ekibin bir planı vardır ve kaynak harcayan her
işlem bu plana göre denetlenir. Kötüye kullanıma karşı istekler hız sınırına tabidir, yöneticiler ekipleri ve
kullanıcıları askıya alabilir.

**Planlar.** `hobby` varsayılandır: açık kayıtla gelen kullanıcının kişisel ekibi ve sonradan kurulan her ekip
Hobby'de başlar. `pro` ve `unlimited` (arayüzde "Sınırsız") yalnızca bir platform yöneticisi tarafından atanır.
Migration 016'dan önce var olan ekipler (izin listeleriyle kurulmuş kurulumlar) `unlimited`'e alınır; mevcut
kurulumlar yükseltmeden sonra kısıtlanmaz.

| Limit (anahtar) | Hobby | Pro | Sınırsız | Nerede uygulanır |
|---|---|---|---|---|
| Proje (`projects`) | 10 | 100 | ∞ | Proje oluşturma ve GitHub'dan import (API + arayüz) |
| Eşzamanlı build (`concurrent_builds`) | 1 | 4 | ∞ | Worker kuyruğu: sınırdaki ekip atlanır, diğerleri beklemez |
| Aylık build dakikası (`build_minutes`) | 600 | 6000 | ∞ | Yeni deploy (push, deploy hook'u, yeniden deploy, preview'i production'a taşıma, import) |
| Build zaman aşımı, dk (`build_timeout_minutes`) | 15 | 45 | yok | Worker'ın build aşaması (`PAAS_DEPLOY_TIMEOUT` ayrıca geçerli) |
| Günlük deploy (`deployments_per_day`) | 50 | 1000 | ∞ | Yeni deploy (yukarıdakilerle aynı) |
| Özel alan adı (`domains`) | 3 | 50 | ∞ | Alan adı ekleme (API + arayüz) |
| Veritabanı (`addons`, Faz 22) | 2 | 20 | ∞ | Eklenti oluşturma (API + arayüz) |
| Süreç başına kopya (`replicas`, Faz 20) | 2 | 10 | ∞ | Worker ölçekleme (API + arayüz) ve build sonrası `paas.yaml` |
| Cron işi (`crons`, Faz 20) | 2 | 10 | ∞ | Build sonrası `paas.yaml` |
| Proje başına CPU kotası (`cpu`) | 1 | 4 | `PAAS_APP_QUOTA_CPU` | Namespace `ResourceQuota` (`requests.cpu`) |
| Proje başına bellek kotası (`memory`) | 2Gi | 8Gi | `PAAS_APP_QUOTA_MEMORY` | `ResourceQuota` (`requests.memory`, `limits.memory`) ve `LimitRange` varsayılanının üst sınırı |

Her değer ortam değişkeniyle değişir: `PAAS_PLAN_<PLAN>_<ANAHTAR>`, ör. `PAAS_PLAN_HOBBY_PROJECTS=5`,
`PAAS_PLAN_PRO_BUILD_MINUTES=unlimited`, `PAAS_PLAN_HOBBY_MEMORY=1Gi`. Bir yönetici tek bir ekip için tek tek
limitleri de değiştirebilir (Yönetim sayfasında `projects=50` gibi satırlar ya da API'de `"limits"` nesnesi); yazılmayan
anahtarlar planı izler. Günlük ve aylık pencereler UTC takvimine göredir. Build süresi deploy başına saklanır
(`deployments.build_seconds`, her deneme eklenir; imajı yeniden kullanan deploy'lar build dakikası harcamaz); aylık
toplam bu ay oluşturulan deploy'ların toplamıdır ve yukarı yuvarlanmış dakika olarak gösterilir. Eklentilerin
kotası (Faz 22) plan kotasının üstüne eklenir.

**Reddetme.** Sayılarla ilgili limitler `403`, sıfırlanan pencereler (günlük deploy, aylık dakika) `429` döner:

```json
{"error": "plan limit reached: the Hobby plan allows 50 deployments per day", "limit": "deployments_per_day", "plan": "hobby", "max": 50, "used": 50}
```

Arayüz aynı hataları Türkçe gösterir ("Plan limitine ulaşıldı: Hobby planında günde en fazla 50 deploy
yapılabilir…"). Limit yüzünden kuyruğa alınmayan bir push, webhook'a `{"result": "refused", …}` ile cevaplanır ve
commit GitHub'da kırmızı **"Plan limiti aşıldı (limit reached)"** durumu alır (bağlantısı Kullanım sayfası). Import
proje limitine takılırsa proje oluşmaz; yalnızca günlük deploy ya da aylık dakika doluysa proje oluşur ama ilk
deploy başlamaz (uyarıyla). Planı aşan bir `paas.yaml` (fazla cron ya da kopya) deploy'u nedeniyle `failed` yapar.

**Adil kuyruk.** Worker sırada en eski deploy'u değil, eşzamanlı build sınırının altındaki ekiplerin en eski
deploy'unu alır (`ClaimNextFor`); sınırındaki bir ekibin deploy'ları sırada bekler ama başka ekipleri tıkamaz.
Seçim kısa bir advisory lock altında yapılır, iki worker aynı ekibi sınırın üstüne çıkaramaz.

**Askıya alma.** Askıya alınan ekip deploy edemez ve yeni kaynak oluşturamaz; sıradaki deploy'ları iptal edilir,
süren deploy'ları durdurulur, uygulamaları sıfıra ölçeklenir (Deployment / StatefulSet kopya sayıları açıklamaya
yazılır, CronJob'lar `suspend` edilir, namespace kotası `pods: 0` olur). Nesneler, diskler ve veriler korunur;
askı kaldırılınca her şey kayıtlı kopya sayısıyla geri gelir. Askıya alınan kullanıcı giriş yapamaz, oturumu
kapanır, token'ları `403` alır. Platform yöneticileri askıya alınamaz.

**Kötüye kullanım koruması.** Bellekte token bucket (her kontrol düzlemi kopyası kendi payını sayar):

| Değişken | Varsayılan | Ne sınırlanır |
|---|---|---|
| `PAAS_RATE_LIMIT_SIGNIN` | `20/1m` | GitHub girişi ve token girişi, IP başına |
| `PAAS_RATE_LIMIT_IMPORT` | `20/1h` | Proje oluşturma ve import, kullanıcı ve IP başına |
| `PAAS_RATE_LIMIT_API_WRITES` | `120/1m` | Her durum değiştiren API isteği ve arayüz formu, kullanıcı ve IP başına (okumalar serbest) |
| `PAAS_TRUST_PROXY_HEADERS` | `false` | `true`: istemci IP'si `X-Forwarded-For`'un son girdisinden (Traefik arkasında) |
| `PAAS_SIGNUP_MIN_ACCOUNT_AGE` | `0` (kapalı) | Açık kayıtta bundan genç GitHub hesapları reddedilir (`720h` ya da `30d`; GitHub `/user` yanıtındaki `created_at`). Yöneticiler, `PAAS_ALLOWED_GITHUB_USERS` ve bir ekibe davet edilmiş hesaplar etkilenmez |

Değerler `<sayı>/<süre>` ya da `off`. Sınırı aşan istek `429` ve `Retry-After` alır.

**Yönetim.** Platform yöneticisi `PAAS_ADMIN_GITHUB_LOGINS`'teki bir GitHub kullanıcısı ya da eski admin token'ıdır.
Arayüzde üst menüde **Yönetim** (`/admin`): tüm ekipler kullanımlarıyla (çubuklar), plan ve özel limit formu,
askıya alma / kaldırma, kullanıcı askıya alma. API:

| Yöntem | Yol | Açıklama |
|---|---|---|
| GET | `/api/teams/{slug}/usage` | Ekibin planı, geçerli limitleri ve kullanımı (viewer) |
| GET | `/api/admin/teams` | Tüm ekipler: plan, limitler, kullanım, üye sayısı, sahipler |
| GET | `/api/admin/teams/{slug}` | Tek ekip |
| PUT | `/api/admin/teams/{slug}/plan` | `{"plan":"pro","limits":{"projects":50,"memory":"4Gi"}}`; `limits` yazılmazsa özel limitler kalır, `null` / `{}` temizler |
| POST | `/api/admin/teams/{slug}/suspend` | `{"reason"?}`: askıya alır, uygulamaları sıfıra ölçekler |
| POST | `/api/admin/teams/{slug}/unsuspend` | Askıyı kaldırır, uygulamaları geri ölçekler |
| GET | `/api/admin/users/suspended` | Askıdaki kullanıcılar |
| POST | `/api/admin/users/{login}/suspend` | `{"reason"?}` |
| POST | `/api/admin/users/{login}/unsuspend` | Askıyı kaldırır |

**Kullanım sayfası.** Üst menüdeki **Kullanım** (`/usage?team=<slug>`, ekip sayfasından da bağlantılı): projeler, bu
ayki build dakikası, bugünkü deploy'lar, veritabanları, alan adları ve süren build'ler, limitlerine göre çubuklarla
(%80'de sarı, dolunca kırmızı); altında build zaman aşımı, kopya, cron ve kota limitleri.

**Yasal sayfalar.** `/terms` (Kullanım koşulları) ve `/privacy` (Gizlilik politikası, KVKK'ya göre: saklanan veriler
— GitHub kullanıcı adı, adı, avatarı, şifreli ortam değişkenleri, loglar —, amaçlar, saklama süreleri, haklar)
girişsiz açılır; her sayfanın alt bilgisinde ve giriş sayfasında ("Giriş yaparak koşulları kabul edersin")
bağlantılıdır. `PAAS_CONTACT_EMAIL` başvuru adresini, `PAAS_OPERATOR_NAME` platformu işleteni adlandırır. Metinler
şablondur; yayına almadan önce bir hukukçuya gözden geçirtin (not şablonun içinde yorum olarak durur, sayfada
görünmez).

**Herkese açık kurulum için kontrol listesi.** `PAAS_OPEN_SIGNUP=true`; GitHub App ayarlarında *Where can this
GitHub App be installed?* → **Any account** (yoksa başka hesaplar repolarını bağlayamaz, bkz.
[GitHub App](#github-app-faz-15)); `PAAS_ADMIN_GITHUB_LOGINS` ile en az bir yönetici; Traefik arkasında
`PAAS_TRUST_PROXY_HEADERS=true`; `PAAS_CONTACT_EMAIL` ve gözden geçirilmiş yasal metinler; gerekirse
`PAAS_SIGNUP_MIN_ACCOUNT_AGE=30d`.


## Proje silme (Faz 28)

Ayarlar › Tehlikeli bölge › **Projeyi sil**: sayfa neyin silineceğini (deploy'lar ve adresleri, alan adları, veritabanları
ve verileri, ortam değişkenleri, ayarlar) ve geri alınamayacağını söyler; onay için proje adı yazılır. JS varken düğme ad
eşleşene kadar kapalıdır, JS olmadan sunucu adı kontrol eder. Başarıda `/` açılır: "„blog“ projesi silindi."

- **Kim:** ekibin owner'ı, platform yöneticileri (`PAAS_ADMIN_GITHUB_LOGINS`) ve admin token'ı. Member / viewer `403`
  (arayüzde gerekçeli kapalı düğme), başka ekip `404`.
- **API:** `DELETE /api/apps/{name}` gövdesi `{"confirm":"<ad>"}`; ad tutmazsa `400`. Hiçbir deploy çalışmıyorsa ve namespace
  hemen kalkıyorsa `204`; çalışan deploy'lar duruyor ya da namespace sonlanıyorsa `202` (`{"app","status":"deleting","deletion_id"}`),
  arka plan döngüsü saniyeler içinde bitirir. Uygulama her iki durumda da API'de, listelerde ve push'larda hemen yok olur.
- **CLI:** `paas apps delete <app>` adı sorar; `--yes` sormaz.
- **Adımlar** (`internal/appdelete`, migration 021; her adım tekrarlanabilir, çökme sonrası döngü kaldığı yerden sürer):
  1. Tek işlemde: `app_deletions` kaydı (kim, ne zaman), `apps.deleting_at` (push, deploy hook, redeploy, API ve listeler
     uygulamayı görmez; kuyruğa yeni iş girmez, worker almaz), kuyruktaki deploy'lar iptal, çalışanlara Faz 17 iptal isteği,
     `audit_log`'a `app_delete` satırı. `project.deleted` bildirimi gönderilir.
  2. Hiçbir deploy `building` / `deploying` değilken tek işlemde uygulama satırı silinir; ona bağlı her tablo CASCADE ile gider
     (deploy'lar ve logları, alias'lar, değişkenler, alan adları, hook'lar, build / süreç / yayın / koruma ayarları, rollout'lar,
     metrikler, eklentiler + kopya ve yedek kayıtları, yorumlar, bildirim süzgeçleri). Yalnızca bu projeyi izleyen bildirim
     kanalları kapatılır (süzgeçsiz kanal bütün projeleri alır).
  3. Küme: `app-<ad>` namespace'i ve uygulamayla etiketli diğer namespace'ler silinir; içindeki her şey (Deployment, Service,
     Ingress, IngressRoute / TraefikService / Middleware, süreçler, CronJob'lar, eklenti StatefulSet / PVC / yedek ve kopya
     Job'ları, env Secret'ları, cert-manager Certificate ve Secret'ları) onunla gider. Namespace dışında kalan tek şey `Retain`
     politikalı PersistentVolume'lardır; bunlar ayrıca silinir (RBAC'e `persistentvolumes` `get/list/delete` eklendi).
     ClusterIssuer, Traefik ve kontrol düzlemi namespace'inde uygulamaya ait nesne yoktur. Namespace tamamen kalkınca kayıt biter.
     `PAAS_DEPLOYER=dryrun` ile küme adımı yoktur.
- **Ad yeniden kullanımı:** ad ve repo, 3. adım bitince serbesttir (çoğunlukla isteğin içinde). O ana kadar aynı adla proje
  oluşturma / import `409` "still being deleted" döner; böylece yeni proje eski namespace'e hiç denk gelmez. Aynı reponun
  yeniden import'u çalışır.
- **Kalanlar:** denetim kaydı (terminal oturumları ve silme satırı) proje adıyla, `app_id` boş olarak saklanır; bildirim
  geçmişi ve `app_deletions` kaydı da kalır. Plan kullanımındaki proje sayısı silme anında düşer. GitHub App kurulumuna ve
  repoya dokunulmaz; PR'lara yazılmış preview yorumları silinmez.


## Web arayüzü ve canlı loglar (Faz 5)

Arayüz `/` adresinde: GitHub ile giriş yapılır (geliştirme modunda API token'ı ile; bkz.
[Kullanıcılar ve ekipler](#kullanıcılar-ve-ekipler-faz-13)). Sayfalar:

- **Uygulamalar:** liste, her birinin son deploy durumu, yeni uygulama formu
- **Uygulama** (Faz 16–19 ekranlarından beri dört sekme):
  - **Genel** (`/apps/{name}`): adresler, production deploy'u, son deploy'lar
  - **Deploy'lar** (`/apps/{name}/deployments`): geçmiş; her deploy kendi adresini (nesil ekiyle, ör.
    `<sha7>-1-<app>`), framework'ünü, ortamını (Production / Önizleme) ve kaynağını (push, yeniden deploy,
    production'a taşıma, deploy hook'u) gösterir. Member+ için "Production'a taşı" (hazır önizlemeler),
    "Bu sürüme dön", "Yeniden deploy et" ("Önbelleği kullanma" seçeneğiyle) ve "İptal et" (sıradaki ve
    süren deploy'lar) düğmeleri
  - **Analitik** (`/apps/{name}/analytics?range=1h|24h|7d`): istek, hata oranı (5xx), p95; istekler ve 5xx
    için sunucuda çizilen SVG grafik (JavaScript yok, her adımın üzerine gelince değerleri görünür); son
    15 dakikanın deploy sağlığı (Sağlıklı / Sorunlu / Çöküyor / Trafik yok); anlık CPU / bellek
    (metrics-server ya da Kubernetes yoksa "Kullanım verisi yok")
  - **Ayarlar** (`/apps/{name}/settings`): build ayarları (framework seçimi, "Otomatik algıla (<algılanan>)",
    kök dizin, komutlar, çıktı dizini, Node sürümü; API ile aynı doğrulama), ortam değişkenleri (Tümü /
    Production / Önizleme, önizleme için isteğe bağlı branch; değerler asla gösterilmez), deploy hook'ları
    (URL yalnızca oluşturulunca bir kez gösterilir), alan adları, uyku modu

  Sekmeler JSON API ile aynı fonksiyonları çağırır (`api.Promote`, `api.Redeploy`, `api.Cancel`,
  `api.Analytics`, `api.Health`, `api.Usage`, `build.NormalizeSettings`); izleyiciler her şeyi salt okunur
  görür, deploy hook'ları yalnızca üyelere listelenir.
- **Deployment** (`/deployments/{id}`): canlı build logu, bitince durum rozeti güncellenir; Kubernetes'te pod logları
- **Ekipler** (`/teams`): ekip oluşturma, üyeler ve roller; **Tokens** (`/tokens`): kişisel API token'ları

Güvenlik: oturum, `PAAS_SESSION_KEY` (yoksa API token'ından türetilen) anahtarla HMAC imzalı, sunucuda
durum tutmayan bir çerezdir (`HttpOnly`, `SameSite=Lax`, HTTPS'te `Secure`); anahtar değişince tüm
oturumlar düşer. `Strict` değil, çünkü GitHub'dan girişten ya da App kurulumundan dönüş siteler arası bir
gezinmedir ve `Strict` her dönüşte oturumu düşürürdü.
CSRF koruması: değişiklik yapan her istek oturuma bağlı HMAC CSRF token'ını (form alanı ya da `X-CSRF-Token`
başlığı) taşımalı ve `Origin` (yoksa `Referer`) aynı host olmalıdır; `Lax` çerezin siteler arası POST'larla
gitmemesi ikinci katmandır.
Sayfalar sıkı bir CSP (istek başına nonce) ile sunulur.

Canlı log: yeni log satırı ve durum değişikliği Postgres'te `NOTIFY` üretir; SSE akışı önce
geçmişi, sonra canlı satırları gönderir, deployment bitince son `status` olayıyla kapanır.
`Last-Event-ID` ile kopan bağlantı kaldığı yerden devam eder.


## CLI (Faz 18)

`paas`, platformun komut satırı istemcisidir (`vercel` CLI'ı örnek alındı). Yalnızca HTTP API'sini kullanır,
standart kütüphane dışında bağımlılığı yoktur. Sunucu ikilisi `cmd/paas`'ta olduğu için istemci
`cmd/paas-cli` dizinindedir ve `paas` adıyla derlenir:

```bash
go build -o paas ./cmd/paas-cli        # Windows: go build -o paas.exe ./cmd/paas-cli
```

**Giriş.** Web arayüzündeki *Tokens* sayfasında (`/tokens`) bir kişisel API token'ı oluşturun, sonra:

```bash
paas login --url https://paas.example.com   # token'ı sorar, GET /api/me ile doğrular
paas whoami                                 # kullanıcı ve ekip rolleri
paas logout                                 # kayıtlı URL ve token'ı siler
```

URL ve token `os.UserConfigDir()/paas/config.json` dosyasına yalnızca kullanıcının okuyabileceği
izinle (`0600`) yazılır: Linux'ta `~/.config/paas/config.json`, macOS'ta
`~/Library/Application Support/paas/config.json`, Windows'ta `%AppData%\paas\config.json`. Kayıtlı
token yalnızca kaydedildiği URL'ye gönderilir; `--url` ile başka bir sunucu verilirse kullanılmaz.
`logout` token'ı sunucuda iptal etmez, bunun için *Tokens* sayfası kullanılır. CI'da giriş gerekmez:

| Öncelik | URL | Token |
|---|---|---|
| 1 | `--url` | `--token` |
| 2 | `PAAS_URL` | `PAAS_TOKEN` |
| 3 | `paas login` ile kaydedilen | kaydedilen (yalnızca aynı URL için) |

**Komutlar**

| Komut | Ne yapar | API |
|---|---|---|
| `paas ls` | Uygulamalar: production URL'si, son deployment, durumu, branch'i, yaşı | `GET /api/apps`, `…/deployments?limit=1` |
| `paas deployments <app> [--limit N]` | Deployment tablosu; production / preview alias'larının hangi deploy'da olduğu, build süresi | `GET /api/apps/{name}/deployments`, `GET /api/apps/{name}` |
| `paas inspect <id>` | Tek deployment: durum, hata, URL, commit, imaj, süreler | `GET /api/deployments/{id}` |
| `paas logs <id>` | O ana kadarki build logu (1000 satırlık sayfalarla) | `GET /api/deployments/{id}/logs?after=` |
| `paas logs <id> -f` | Canlı build logu, deployment bitene kadar; bağlantı koparsa son satırdan devam eder | `GET /api/deployments/{id}/logs/stream` (SSE) |
| `paas logs --runtime <app> <id> [-f] [--tail N]` | Pod logları | `GET /api/apps/{name}/deployments/{id}/runtime-logs` |
| `paas rollback <app> <id>` | Production alias'ını eski bir deployment'a çevirir | `POST /api/apps/{name}/rollback` |
| `paas env ls <app>` | Değişken adları (değerler API'den okunamaz) | `GET /api/apps/{name}/env` |
| `paas env set <app> KEY=VALUE…` | Değişken ekler / değiştirir | `PUT /api/apps/{name}/env` |
| `paas env rm <app> KEY…` | Değişken siler (`null` gönderir) | `PUT /api/apps/{name}/env` |
| `paas domains ls\|add\|verify\|rm <app> [host]` | Özel alan adları; `add` oluşturulacak DNS kayıtlarını gösterir | `/api/apps/{name}/domains…` |
| `paas import <owner/repo> [--name n] [--team t] [--branch b]` | GitHub App kurulumundaki repoyu import eder, ilk deploy'u kuyruğa koyar | `POST /api/apps/import` |
| `paas open <app> [--print]` | Production URL'sini yazdırır ve tarayıcıda açar | `GET /api/apps/{name}` |
| `paas addons ls\|add\|info\|set\|rm\|rotate\|branches\|reset\|backups\|backup\|restore <app> …` | Veritabanları (Faz 22); `info --reveal` şifreyi gösterir (loglanır), `rm` / `restore` `--confirm <ad>` ister | `/api/apps/{name}/addons…` |
| `paas db psql <app> [addon] [--branch b]` | `kubectl port-forward` ve `psql` komutlarını yazar (tünel açmaz) | `POST …/addons/{addon}/credentials/reveal` |
| `paas ps <app>` / `paas ps scale <app> worker=3` | Süreçler (web, worker, cron) ve worker ölçekleme (Faz 20); `worker=default` paas.yaml'a döner | `GET /api/apps/{name}/processes`, `PUT …/processes/{proc}` |
| `paas cron run <app> <cron>` | Production'daki cron işini hemen başlatır | `POST /api/apps/{name}/crons/{cron}/run` |
| `paas rollout status\|settings\|promote\|pause\|resume\|rollback\|abort <app>` | Kademeli yayın durumu, ayarları ve elle yönetimi (Faz 21) | `/api/apps/{name}/rollout…` |
| `paas apps delete <app> [--yes]` | Projeyi her şeyiyle siler; adı sorar (Faz 28) | `DELETE /api/apps/{name}` |

Örnek akış:

```bash
paas import acme/web --team default
# Imported acme/web as web (production branch main).
# First deployment #21 (0123456) is queued. Follow it with: paas logs 21 -f
paas logs 21 -f            # deployment ready → çıkış kodu 0, failed → 1
paas env set web DATABASE_URL=postgres://… DEBUG=1
paas deployments web
paas rollback web 18
```

**Ortak bayraklar ve çıktı.** `--url`, `--token` ve `--json` komuttan önce de sonra da yazılabilir
(`paas --json ls`, `paas ls --json`). `--json` API yanıtını olduğu gibi (girintili) yazar; `ls` her uygulamaya
`last_deployment` ekler, `logs -f --json` her satırı ve durum olayını ayrı bir JSON satırı (NDJSON) olarak
verir. Tablolar `text/tabwriter` ile hizalanır, zamanlar göreli yazılır (`5m ago`, `2d ago`).

**Çıkış kodları ve hatalar.** `0` başarılı, `1` hata, `2` hatalı kullanım (eksik argüman, bilinmeyen bayrak
veya komut). API hataları sunucunun mesajı ve HTTP koduyla, altında bir ipucuyla yazılır: **401** →
token'ın süresi dolmuş veya iptal edilmiş, `/tokens`'ta yenisini oluşturup `paas login`; **403** → ekipteki
rol yetmiyor (okuma viewer, değişiklik member ister); **404** → adı / id'yi `paas ls` ile kontrol edin,
başka ekibin uygulamaları da 404 döner; sunucuya ulaşılamazsa URL'yi kontrol etme ipucu.

**Testler.** `internal/cli` paketi `httptest` ile sahte bir API sunucusuna karşı test edilir: giriş ve
yapılandırma dosyasının yazılması (geçici dizin, `APPDATA` / `HOME` / `XDG_CONFIG_HOME`), token'ın başka
sunucuya gönderilmemesi, `ls`, SSE üzerinden `logs -f` (kopan bağlantıdan `after=` ile devam, `failed` →
çıkış kodu 1), log sayfalama, pod logları, `env set` / `rm` istek gövdeleri, rollback, import, alan adları,
401 / 403 / 404 / 500 ve kullanım hatalarının çıkış kodlarına eşlenmesi.

```bash
go test ./cmd/paas-cli/... ./internal/cli/...
```


## API

Tüm `/api/*` uçları `Authorization: Bearer <token>` ister: kişisel token (`paas_…`) veya admin token'ı
(`PAAS_API_TOKEN`). GET istekleri arayüzün oturum çereziyle de çalışır. Uygulama uçları ekip rolüne
göre yetkilendirilir (viewer okur, member değiştirir; bkz. [roller](#kullanıcılar-ve-ekipler-faz-13)).

| Yöntem | Yol | Açıklama |
|---|---|---|
| GET | `/healthz` | Sağlık kontrolü (veritabanına ping atar) |
| POST | `/webhooks/github` | GitHub webhook alıcısı (imza ile korunur) |
| POST | `/api/apps` | `{"name","repo","production_branch"?,"team"?}` — ekip verilmezse member olunan ilk ekip |
| GET | `/api/apps` | Uygulamalar |
| GET | `/api/github/repos` | Ekiplerin GitHub App kurulumlarındaki repolar (`team`, `can_import`; zaten import edildiyse `app`) |
| POST | `/api/apps/import` | `{"repo","name"?,"team"?,"production_branch"?}` — member+; uygulamayı oluşturur, ilk deploy'u kuyruğa koyar (`201`, `{"app","deployment"?,"warning"?}`); import edilemeyen repo `404`, çakışma `409` |
| GET | `/api/apps/{name}` | Uygulama + alias'lar |
| DELETE | `/api/apps/{name}` | `{"confirm":"<ad>"}` — projeyi her şeyiyle siler (owner / platform yöneticisi); bittiyse `204`, arka planda sürüyorsa `202`; ad tutmazsa `400` ([Faz 28](#proje-silme-faz-28)) |
| GET | `/api/apps/{name}/deployments?limit=` | Deployment geçmişi |
| POST | `/api/apps/{name}/rollback` | `{"deployment_id"}` — production alias'ını taşır |
| GET | `/api/apps/{name}/env` | `{"keys":[…],"vars":[{"key","target","git_branch"?,"updated_at"}]}` — değerler API'den asla okunmaz |
| PUT | `/api/apps/{name}/env` | `{"KEY":"değer","ESKI":null}` (iki ortam) ya da `{"target":"production\|preview","git_branch"?,"vars":{…}}` — birleştirir, `null` siler. `PORT` ve `PAAS_*` platforma ait |
| POST | `/api/apps/{name}/promote` | `{"deployment_id"}` — hazır preview'i build'siz production'a alır (`202`, yeni deploy) ya da production ortamındaki deploy'a alias'ı taşır (`200`) |
| POST | `/api/apps/{name}/deployments/{id}/redeploy` | `{"use_cache"?: bool}` — aynı commit; varsayılan imajı yeniden kullanır, `false` yeniden build eder (`202`) |
| POST | `/api/deployments/{id}/cancel` | Kuyruktakini hemen iptal eder (`200`), çalışana iptal isteği yazar (`202`); bitmişse `409` (member+) |
| GET | `/api/apps/{name}/hooks` | Deploy hook'ları (token'sız; member+) |
| POST | `/api/apps/{name}/hooks` | `{"name"?,"branch"?}` — `token` ve `path` yalnızca bu yanıtta (`201`; member+) |
| DELETE | `/api/apps/{name}/hooks/{id}` | Hook'u siler (`204`; member+) |
| POST | `/api/hooks/deploy/{token}` | Kimlik doğrulamasız; hook'un branch'inin son commit'ini deploy eder (`201`/`200`; GitHub App yoksa `501`) |
| GET | `/api/apps/{name}/scale-to-zero` | `{"production": bool}` — production deploy'u boştayken uyuyabilir mi |
| PUT | `/api/apps/{name}/scale-to-zero` | `{"production": true\|false}` (preview'ler her zaman uyuyabilir) |
| GET | `/api/apps/{name}/analytics?range=1h\|24h\|7d&deployment=` | İstek analitiği: zaman serisi (istek, 2xx–5xx, hata, p50/p95/ortalama ms) + toplamlar + deploy dağılımı |
| GET | `/api/apps/{name}/health` | Deploy başına son 15 dakikanın 5xx oranı ve durumu (`healthy` / `degraded` / `failing` / `no_traffic`) |
| GET | `/api/apps/{name}/usage` | Çalışan deploy pod'larının anlık CPU / bellek kullanımı ve limitleri (metrics-server; yalnızca `kubernetes` deployer) |
| GET | `/api/apps/{name}/rollout-settings` | Yayın ayarları: `mode`, `steps`, `step_seconds`, eşikler, `guard_seconds` (viewer) |
| PUT | `/api/apps/{name}/rollout-settings` | Gönderilen alanları birleştirir; geçersiz değer `400` (member) |
| GET | `/api/apps/{name}/rollout` | `{"settings","active","recent"}` — etkin yayın iki tarafın metrikleri ve sıradaki kararla (viewer) |
| POST | `/api/apps/{name}/rollout/{promote\|abort\|pause\|resume\|rollback}` | Etkin yayına elle işlem; yayın yoksa `404`, duruma uymuyorsa `409` (member) |
| GET | `/api/apps/{name}/addons` | Veritabanları: tür, boyut, durum, önizleme modu, kurallar, sunucu, verdikleri değişken adları — şifresiz (viewer) |
| POST | `/api/apps/{name}/addons` | `{"kind":"postgres\|redis","name"?,"plan"?,"preview_mode"?}` (`201`, `provisioning`); aynı ad `409`, en fazla 5 (member) |
| GET | `/api/apps/{name}/addons/{addon}` | Tek eklenti (viewer) |
| PATCH | `/api/apps/{name}/addons/{addon}` | `{"plan"?,"preview_mode"?,"anonymize"?:[…],"anonymize_sql"?,"backup_keep"?}`; geçersiz kural / küçülen disk `400` (member) |
| DELETE | `/api/apps/{name}/addons/{addon}?confirm=<addon>` | Eklentiyi diskleri, kopyaları ve yedekleriyle siler (`202`); onaysız `400` (member) |
| POST | `/api/apps/{name}/addons/{addon}/credentials/reveal` | `{"branch"?}` — bağlantı bilgileri ve şifre; her görüntüleme loglanır (member) |
| POST | `/api/apps/{name}/addons/{addon}/rotate` | `{"redeploy"?: true}` — yeni şifre; uygulanınca alias'ların gösterdiği deploy'lar yeniden deploy edilir (`202`; member) |
| GET | `/api/apps/{name}/addons/{addon}/branches` | Preview kopyaları: branch, veritabanı, mod, durum, anlık görüntü, boyut, hata / uyarı (viewer) |
| POST | `/api/apps/{name}/addons/{addon}/branches/{branch}/reset` | Kopyayı production'dan yenile (`feature%2Fx`; `202`, sürerken `409`; member) |
| GET | `/api/apps/{name}/addons/{addon}/backups` | Yedekler ve geri yükleme durumları (viewer) |
| POST | `/api/apps/{name}/addons/{addon}/backups` | Şimdi yedekle (`202`; member) |
| POST | `/api/apps/{name}/addons/{addon}/backups/{id}/restore` | `{"confirm":"<addon>"}` — yedeği production'a geri yükler (`202`; member) |
| GET | `/api/apps/{name}/protection` | Deploy koruması: `mode`, `scope`, `password_set`, `bypass` (önek), `share_links` — gizli değerler olmadan (viewer) |
| PUT | `/api/apps/{name}/protection` | `{"mode"?:"off\|team\|password","scope"?:"previews\|all","password"?}` — boş alan değişmez; şifre 8-256 karakter; yönlendirmeyi hemen günceller (member) |
| POST | `/api/apps/{name}/protection/share-links` | `{"name"?,"expires_in_hours"?,"url"?}` — `token` ve `url` (`?paas_share=…`) yalnızca bu yanıtta (`201`; member) |
| DELETE | `/api/apps/{name}/protection/share-links/{id}` | Paylaşım bağlantısını iptal eder (`204`; member) |
| POST | `/api/apps/{name}/protection/bypass` | Yeni otomasyon anahtarı; `secret` yalnızca bu yanıtta, eskisi geçersiz (`201`; member) |
| DELETE | `/api/apps/{name}/protection/bypass` | Otomasyon anahtarını siler (`204`; member) |
| GET | `/_auth/verify?app=` | Traefik ForwardAuth ucu (kimlik doğrulamasız; `X-Forwarded-*` başlıklarına bakar) |
| GET/POST | `/_protect/login?app=&next=` | Korunan adreslerin giriş sayfası (ekip oturumu ya da şifre) |
| GET | `/api/apps/{name}/settings` | Build ayarları + `detected_framework` + kabul edilen `frameworks` listesi (viewer) |
| PUT | `/api/apps/{name}/settings` | `{"root_directory"?,"framework"?,"install_command"?,"build_command"?,"start_command"?,"output_directory"?,"node_version"?}` — gönderilenleri birleştirir, `""` sıfırlar; geçersiz değer `400` (member) |
| GET | `/api/deployments/{id}` | Tek deployment |
| GET | `/api/deployments/{id}/logs?after=` | Log satırları |
| GET | `/api/deployments/{id}/logs/stream` | Canlı log (SSE); tarayıcıda oturum çereziyle de çalışır |
| GET | `/api/apps/{name}/deployments/{id}/runtime-logs?follow=1&tail=200&process=` | Pod logları (yalnızca `kubernetes` deployer); `process`: `web` (varsayılan), bir worker ya da cron (son çalışması) |
| GET | `/api/apps/{name}/processes?deployment=` | Production'ın (ya da verilen deploy'un) süreçleri: web / worker'lar (komut, paas.yaml kopyası, `override`, istenen / hazır, durum) ve cron'lar (zamanlama, askıda mı, son çalışma) |
| PUT | `/api/apps/{name}/processes/{proc}` | `{"replicas": 0-10}` ya da `{"replicas": null}` (paas.yaml'a dön) — production worker'ı için kopya sayısı; hemen uygulanır (member) |
| POST | `/api/apps/{name}/crons/{cron}/run` | Production'daki cron'u şimdi çalıştırır (`202`, `{"job"}`); çalışan varsa `409` (member) |
| GET | `/api/apps/{name}/domains` | Özel alan adları: durum, hata, oluşturulacak DNS kayıtları |
| POST | `/api/apps/{name}/domains` | `{"hostname"}` — ekler ve hemen bir kez denetler (`201`) |
| POST | `/api/apps/{name}/domains/{hostname}/verify` | DNS'i şimdi denetler |
| DELETE | `/api/apps/{name}/domains/{hostname}` | Alan adını ve `Ingress`'ini kaldırır (`204`) |
| GET | `/api/me` | Çağıran ve ekipleri (rolleriyle) |
| GET | `/api/teams` | Görülebilen ekipler |
| POST | `/api/teams` | `{"slug","name"?}` — oluşturan owner olur |
| GET | `/api/teams/{slug}` | Ekip + üyeler (viewer) |
| PUT | `/api/teams/{slug}/members/{login}` | `{"role":"owner\|member\|viewer"}` — GitHub kullanıcısı ekler / rolü değiştirir (owner) |
| DELETE | `/api/teams/{slug}/members/{login}` | Üyeyi çıkarır (owner; herkes kendisi ayrılabilir) |
| GET | `/api/tokens` | Kişisel token'lar (değerleri değil) |
| POST | `/api/tokens` | `{"name","expires_in_days"?}` — `token` alanı yalnızca bu yanıtta |
| DELETE | `/api/tokens/{id}` | Token'ı iptal eder |
| GET | `/api/teams/{slug}/usage` | Ekibin planı, limitleri ve kullanımı (viewer; Faz 23) |
| GET / PUT / POST | `/api/admin/…` | Planlar, özel limitler, askıya alma (yalnızca platform yöneticisi; bkz. [Faz 23](#planlar-ve-limitler-faz-23)) |
| GET | `/api/deployments/{id}/comments?status=open\|resolved` | Preview yorum dizileri (yanıtlarıyla) ve sayılar (viewer) |
| POST | `/api/deployments/{id}/comments` | `{"body","path"?,"x"?,"y"?,"viewport_width"?}` — yeni dizi; yalnızca hazır deploy (`201`; viewer) |
| POST | `/api/comments/{id}/replies` | `{"body"}` — yanıt (`201`; viewer) |
| POST | `/api/comments/{id}/resolve` | Diziyi çözer (member ya da dizinin yazarı) |
| POST | `/api/comments/{id}/reopen` | Diziyi yeniden açar (member ya da dizinin yazarı) |
| DELETE | `/api/comments/{id}` | Yorumu siler; dizi yanıtlarıyla gider (`204`; yazar ya da owner) |
| GET | `/api/apps/{name}/comments?status=&limit=` | Uygulamanın tüm deploy'larındaki diziler, yeniden eskiye, ve sayılar (viewer) |
| GET / POST | `/api/teams/{slug}/notifications` | Bildirim kanalları; oluşturma member ister ([Faz 26](#bildirimler-faz-26)) |
| GET / PUT / DELETE | `/api/teams/{slug}/notifications/{id}` | Tek kanal; `POST …/{id}/test` test mesajı, `GET …/{id}/deliveries` gönderim günlüğü |
| GET | `/api/notifications/events` | Olay türleri |
| GET / PUT | `/api/me/notifications` | Kendi e-posta tercihlerin |
| GET | `/apps/{app}/deployments/{id}/terminal` | Terminal sayfası; `Upgrade: websocket` ile kabuk oturumu. Yalnızca oturum çereziyle, member+ ([Faz 27](#tarayıcıdan-terminal-faz-27)) |


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
- **Ortam değişkenleri Postgres'te şifreli:** API değerleri asla geri döndürmez; veritabanında AES-256-GCM
  ile şifreli (uygulama + değişken adına bağlı), Kubernetes tarafında Secret olarak durur (Faz 10).
- **Sıfıra ölçeklemede KEDA değil, kontrol düzlemi aktivatör:** KEDA HTTP add-on'u her deploy
  için bir HTTPScaledObject, araya giren bir interceptor proxy, ExternalName Service'ler ve
  Traefik'te `allowExternalNameServices` ister; bu da tüm trafiğin (uyanıkken de) ek bir
  atlamadan geçmesi ve kümeye üç yeni bileşen demek. Burada uyku durumu yalnızca Service'in
  uç noktalarını değiştirir: uyanık deploy'un trafiği hiç değişmez, Ingress'lere dokunulmaz
  (alias uzlaştırma, rollback ve özel alan adları aynen çalışır), boşta olma tespiti k3s'te
  zaten açık olan Traefik metriklerinden gelir ve her durum geçişi fake clientset ile test
  edilebilir. Bedeli: aktivatör kontrol düzleminde çalışır; kontrol düzlemi kapalıyken uyuyan
  deploy'lar uyanamaz (uyanık olanlar etkilenmez).
