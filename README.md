# paas

Vercel tarzı, Git tabanlı PaaS.

Her `git push` değişmez bir **deployment** üretir ve kendi URL'sini alır.
`production` yalnızca bir takma addır (alias); rollback, alias'ı eski bir
deployment'a çevirmektir ve yeniden build gerektirmez.

```
git push ──► GitHub webhook ──► API ──► Postgres kuyruğu ──► worker
                                                         │
                         build (BuildKit) ◄──────────────┤
                         deploy (Kubernetes) ◄───────────┤
                         alias + TLS (Traefik) ◄─────────┘
```

| URL | Ne gösterir |
|---|---|
| `https://<sha7>-<app>.<domain>` | Tek bir commit'in deployment'ı (asla değişmez) |
| `https://<branch>-<app>.<domain>` | Branch'in son başarılı deployment'ı (preview) |
| `https://<app>.<domain>` | Production (production branch'in son deployment'ı veya rollback hedefi) |

**Öne çıkanlar:** Dockerfile'sız build (Node, Go, Python, Ruby, Java, statik) · Next.js, Vite, Nuxt, SvelteKit,
Astro, Remix gibi framework preset'leri ve proje ayarları (monorepo kök dizini, komutlar) · commit başına HTTPS URL ·
branch preview'leri ve PR yorumları · birkaç milisaniyelik rollback · canlı build logu ·
web arayüzü · worker, cron ve WebSocket sunucuları (`paas.yaml`) · otomatik temizlik ve çökme sonrası kurtarma ·
`terraform apply` ile AWS'de kurulum.

## Belgeler

| Belge | İçerik |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Bağlam ve konteyner diyagramları, deploy ve rollback akışları, durum makinesi, veri modeli |
| [docs/REPORT.md](docs/REPORT.md) | Teknik rapor: problem, tasarım kararları, güvenlik, test, ölçümler, sınırlar |
| [docs/MEASUREMENTS.md](docs/MEASUREMENTS.md) | Kuyruk ölçeklenmesi, build süreleri, imaj boyutları, rollback gecikmesi |
| [docs/FAILURE-SCENARIOS.md](docs/FAILURE-SCENARIOS.md) | 16 arıza senaryosu, beklenen davranış ve testleri |
| [docs/DEMO.md](docs/DEMO.md) | Canlı demo ve video senaryosu |
| [docs/PHASES.md](docs/PHASES.md) | Faz planı |
| [infra/README.md](infra/README.md) | AWS kurulumu (Terraform, k3s) |
| [loadtest/README.md](loadtest/README.md) | k6 yük testleri |

## Durum

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
- [x] Faz 8: mimari belgesi, teknik rapor, ölçümler ve yeniden üretme script'leri, demo senaryosu
- [x] Yerel gerçek kümede (k3d) uçtan uca doğrulama: deploy, rollback (trafik altında 0 hata), hata ve temizlik senaryoları
- [x] Faz 9: Python (pip / uv / poetry; Django, FastAPI, Flask), Ruby (Rails, Rack), Java (Maven / Gradle) build'leri,
  her dilde `Procfile` `web:` desteği; hepsi sayısal root olmayan kullanıcıyla, örnekleri gerçek build testinde
- [x] Faz 10: env değerleri veritabanında AES-256-GCM ile şifreli, anahtar kimliğiyle rotasyon (`paas-envkey`)
- [x] Faz 11: sıfıra ölçekleme: boştaki deploy'lar 0 replikaya iner, ilk istek bekletilip uyandırılan pod'a iletilir; production uygulama başına isteğe bağlı
- [x] Faz 12: özel alan adları: DNS doğrulaması (CNAME / TXT), alan adı başına Let's Encrypt sertifikası (HTTP-01), production ve rollback'i izler
- [x] Faz 13: kullanıcılar ve ekipler: GitHub ile giriş (OAuth + PKCE), roller (owner / member / viewer),
  kişisel API token'ları, ekip dışı kaynaklar 404; eski `PAAS_API_TOKEN` acil durum admin token'ı
- [x] Faz 14: üretilen Dockerfile'larda `# syntax=` satırı yok; yerleşik BuildKit frontend'i cache mount'u
  destekliyor, her build'den frontend imajı çözümleme adımı düşüyor: ılık build %18–34 kısa
  ([ölçüm](docs/MEASUREMENTS.md#7-daha-hızlı-ılık-build-faz-14))
- [x] Faz 15: GitHub App: arayüzden "Install" → repoları seç → "Import" (uygulama oluşur, ilk deploy başlar);
  kurulumun ekibe bağlanması kullanıcının GitHub token'ıyla doğrulanır, giriş aynı App üzerinden
  ([kurulum rehberi](#github-app-faz-15))
- [x] Faz 16: proje ayarları (kök dizin, framework, install / build / start komutları, çıktı dizini, Node sürümü) ve
  framework preset'leri: Next.js, Vite, Create React App, Astro, SvelteKit, Nuxt, Remix, React Router, NestJS, Express;
  algılanan framework build logunda ve deployment'ta görünür ([ayrıntılar](#proje-ayarları-faz-16))
- [x] Faz 17: ortamlar ve deploy kontrolleri: production / preview (ve branch'e özel) değişkenler, build'siz
  promote ve redeploy, kuyrukta ve çalışırken iptal (`canceled`), deploy hook'ları, `[skip deploy]` / `[skip ci]`
  ([ayrıntılar](#ortamlar-ve-deploy-kontrolleri-faz-17))
- [x] Faz 18: `paas` komut satırı aracı (`vercel` CLI tarzı): kişisel token ile giriş, uygulamalar, deployment'lar,
  canlı build logu (`logs -f`), pod logları, rollback, ortam değişkenleri, alan adları, import ([kullanım](#cli-faz-18))
- [x] Faz 19: analitik: Traefik metriklerinden dakikalık istek / hata / gecikme (p50, p95) serileri, deploy sağlığı
  (son 15 dakikanın 5xx oranı), metrics-server'dan canlı CPU / bellek kullanımı
- [x] Arayüz (Faz 16–19 ekranları): proje sekmeleri (Genel / Deploy'lar / Analitik / Ayarlar): build ayarları, ortama özel
  değişkenler, deploy hook'ları, promote / redeploy / iptal düğmeleri, sunucuda çizilen trafik grafiği, deploy sağlığı ve
  canlı kaynak kullanımı ([ayrıntılar](#web-arayüzü-ve-canlı-loglar-faz-5))
- [x] Faz 20: süreç tipleri: `paas.yaml` / `Procfile` ile web, worker ve cron; web'siz uygulamalar (bot, kuyruk işleyici),
  yalnızca production neslinde çalışan worker / cron (rollback onları da taşır), kopya sayısı, "şimdi çalıştır", süreç logları,
  uyanan deploy'a WebSocket ([ayrıntılar](#süreç-tipleri-faz-20))
- [ ] AWS'de kurulum ve ölçümlerin hedef sunucuda tekrarı (bkz. [rapor §9](docs/REPORT.md#9-sınırlar-ve-açık-konular))

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

### Özel alan adları (Faz 12)

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
`/.well-known/acme-challenge/` yolunu hariç tutar ([infra/k8s/40-tls.yaml](infra/k8s/40-tls.yaml)).
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

### Sıfıra ölçekleme (Faz 11)

`PAAS_SCALE_TO_ZERO_AFTER` (varsayılan `30m`) boyunca istek almayan deploy'lar 0 replikaya iner;
ilk istek gelince uyanır. İstek bekletilir, pod hazır olunca ona iletilir: kullanıcı hata sayfası
görmez, yalnızca ilk yanıt birkaç saniye gecikir (yerel k3d'de 4–7 s, bkz.
[MEASUREMENTS.md](docs/MEASUREMENTS.md#6-sıfıra-ölçekleme-k3d)). Preview'ler ve production
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
   [05-traefik-config.yaml](infra/k8s/05-traefik-config.yaml) ile açılır, bkz. [Analitik](#analitik-faz-19)).
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

### Analitik (Faz 19)

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
analitik boş kalır. [infra/k8s/05-traefik-config.yaml](infra/k8s/05-traefik-config.yaml) k3s'in
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
Kontrol düzleminin ClusterRole'üne `metrics.k8s.io/pods` (get, list) eklendi.

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

### Build nasıl çalışır

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
   `$PORT` genişler); Go ve statik imajlarda kabuk olmadığından yok sayılır. Örnekler: [examples/](examples/)
3. Build logunda önce algılanan framework (`==> framework: Next.js`), ardından üretilen Dockerfile aynen görünür.
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

### Proje ayarları (Faz 16)

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
[examples/nextjs-hello](examples/nextjs-hello), [examples/vite-hello](examples/vite-hello).

### Web arayüzü ve canlı loglar (Faz 5)

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
durum tutmayan bir çerezdir (`HttpOnly`, `SameSite=Strict`, HTTPS'te `Secure`); anahtar değişince tüm
oturumlar düşer.
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

### Ortam değişkenlerinin şifrelenmesi (Faz 10)

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

### Ortamlar ve deploy kontrolleri (Faz 17)

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
yeniden build ettirir. Kuyrukta ya da çalışmakta olan bir deploy için `409` döner.

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

### Kullanıcılar ve ekipler (Faz 13)

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

### GitHub App (Faz 15)

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

- GitHub'dan dönüş siteler arası bir yönlendirmedir, `SameSite=Strict` oturum çerezi gönderilmez. Akışı
  kimin başlattığını imzalı state, GitHub'da kimin onayladığını token söyler; ikisi aynı hesap olmalıdır.
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

### CLI (Faz 18)

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

### Süreç tipleri (Faz 20)

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

Örnekler: [`examples/websocket-chat`](examples/websocket-chat) ve [`examples/worker-queue`](examples/worker-queue).

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

### Yerel Kubernetes (k3d)

Gerekenler: Docker, [k3d](https://k3d.io), kubectl.

```bash
scripts/k3d-up.sh     # k3s + Traefik + registry (localhost:5111), port 80
```

`.env` içinde:

```
PAAS_BUILDER=docker
PAAS_REGISTRY=localhost:5111
PAAS_DEPLOYER=kubernetes
PAAS_INGRESS_TLS=false
PAAS_DOMAIN=localtest.me
```

`make run` sonrası push edilen her commit `http://<sha7>-<app>.localtest.me` adresinde açılır.
Kaldırmak için: `scripts/k3d-up.sh down`.

### Gerçek GitHub'a bağlamak

GitHub App ile repo başına ayar gerekmez: bkz. [GitHub App (Faz 15)](#github-app-faz-15). App'siz yol:

Repo → Settings → Webhooks → Add webhook:
- **Payload URL:** `https://<sunucun>/webhooks/github` (yerelde test için ngrok veya Cloudflare Tunnel)
- **Content type:** `application/json`
- **Secret:** `PAAS_GITHUB_WEBHOOK_SECRET` ile aynı
- **Events:** "Let me select individual events" → **Pushes** ve **Pull requests**
  (PR kapanınca preview temizliği için)

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

## Proje yapısı

```
cmd/paas/             giriş noktası: API, worker'lar, routing, cleanup tek süreçte; graceful shutdown
cmd/paas-dockerfile/  bir dizin için üretilecek Dockerfile'ı yazdırır (go run ./cmd/paas-dockerfile <dizin>)
cmd/paas-envkey/      env şifreleme anahtarı üretir, durumunu gösterir, rotasyon yapar
cmd/paas-cli/         `paas` komut satırı istemcisi (go build -o paas ./cmd/paas-cli)
internal/config/      ortam değişkenleri
internal/api/         HTTP uçları, SSE log akışı
internal/webhook/     GitHub imza doğrulama, push / pull_request ayrıştırma
internal/store/       PostgreSQL erişimi, migration'lar, kuyruk, LISTEN/NOTIFY
internal/worker/      kuyruk tüketicisi, Builder/Deployer arayüzleri, heartbeat, kurtarma, dry-run
internal/build/       clone, dil ve framework algılama, proje ayarları, Dockerfile üretimi, BuildKit/docker motorları
internal/deploy/      Kubernetes: namespace, kota, Secret, Deployment, Service, Ingress, rollout, loglar
internal/routing/     alias'ları veritabanından ingress katmanına senkronlar, uzlaştırma döngüsü
internal/process/     süreç tipleri (Faz 20): paas.yaml / Procfile okuma, cron doğrulama, tek nesil kuralı
internal/cleanup/     saklama politikası, emekliye ayırma, küme nesnelerinin silinmesi
internal/scale/       sıfıra ölçekleme: boşta olma tespiti, aktivatör (uyandırma + istek iletimi)
internal/analytics/   istek metrikleri: Traefik sayaçlarından dakikalık farklar, saklama, p50/p95
internal/github/      GitHub REST istemcisi: commit status, PR yorumu
internal/auth/        web oturumu (imzalı çerez) ve CSRF
internal/cli/         CLI komutları: yapılandırma, API istemcisi, SSE okuyucu, tablolar
internal/web/         web arayüzü (html/template + htmx)
internal/naming/      DNS ve Kubernetes için güvenli isimler
internal/secret/      env değerleri için AES-256-GCM, anahtar halkası ve rotasyon
internal/testdb/      testler için temiz veritabanı
examples/             otomatik algılanan örnek uygulamalar (Node, Next.js, Vite, Go, Python, Ruby, Java, statik)
infra/                Terraform (AWS) ve Kubernetes manifest'leri
loadtest/             k6 yük testleri
scripts/              ölçüm script'leri (measure-*.sh), sahte push (push.sh)
docs/                 mimari, rapor, ölçümler, arıza senaryoları, demo
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
- **Ortam değişkenleri Postgres'te düz metin:** API değerleri asla geri döndürmez; Kubernetes
  tarafında Secret olarak durur. Veritabanında şifreleme sonraki bir adım.
