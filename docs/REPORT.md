# paas: Git tabanlı bir PaaS'ın tasarımı ve gerçekleştirilmesi

**Teknik rapor**

## Özet

paas, Vercel tarzı bir uygulama platformudur: bir GitHub reposuna yapılan her `git push`
otomatik olarak build edilir, Kubernetes üzerinde ayrı ve değişmez bir **deployment** olarak
çalıştırılır ve kendi HTTPS adresini alır. Production ve branch preview'leri yalnızca bu
deployment'ları gösteren **takma adlardır (alias)**; rollback, bir takma adı daha önce çalışan
bir deployment'a çevirmektir ve yeniden build gerektirmez.

Sistem Go ile yazılmış tek bir kontrol düzleminden (API, webhook alıcısı, iş kuyruğu,
build ve deploy hattı, yönlendirme, temizlik, web arayüzü), PostgreSQL'den, BuildKit'ten ve
k3s üzerinde Traefik ile cert-manager'dan oluşur. Altyapı Terraform ile AWS'de (EC2 arm64,
Route 53, ECR) sıfırdan kurulur.

Ölçümlerde Postgres tabanlı kuyruk 8 worker'a kadar neredeyse doğrusal ölçeklendi (40 deploy:
1 worker'da 64.5 s, 8 worker'da 9.5 s; hiçbir iş kaybolmadı ya da iki kez işlenmedi), rollback
API'si p95 23 ms'de tamamlandı, cache mount'lu üretilmiş Dockerfile'lar Go projelerinde kod
değişikliği sonrası build süresini soğuk build'in %22'sine indirdi.

---

## 1. Problem

Bir web uygulamasını yayına almak; imaj build etmeyi, bir registry'yi, bir orkestratörü,
yönlendirmeyi, TLS sertifikalarını ve geri almayı içerir. Heroku ve Vercel gibi platformlar bu
zinciri `git push`'a indirger. Bu projenin amacı aynı deneyimi açık bileşenlerle ve tek bir
sunucuda kurmak ve şu soruları yanıtlamaktır:

1. Her commit'i ayrı ve değişmez bir deployment yapmak, rollback'i nasıl basitleştirir?
2. Ayrı bir mesaj kuyruğu olmadan PostgreSQL güvenilir bir iş kuyruğu olabilir mi?
3. Dockerfile yazmayan bir geliştirici için güvenli ve hızlı imajlar otomatik üretilebilir mi?
4. Sistem çöktüğünde, yarım kalan işlerde ve dış servis hatalarında tutarlı kalabilir mi?

## 2. Kapsam

**İlk sürümde olanlar:** GitHub webhook'u ile otomatik deploy; Node, Go ve statik siteler için
dil algılama (ya da reponun kendi Dockerfile'ı); commit başına URL; production ve branch
preview'leri; anında rollback; ortam değişkenleri; canlı build ve çalışma zamanı logları;
GitHub commit status'ları ve PR yorumları; web arayüzü; eski deployment'ların temizlenmesi;
çökme sonrası kurtarma; altyapının kod olarak tanımı.

**Sonraki aşamalar:** çoklu kullanıcı ve ekipler, veritabanı sağlama, özel alan adları,
faturalandırma, daha fazla dil ve framework, çok düğümlü küme, sıfıra ölçekleme.

## 3. Benzer sistemler

| Sistem | Model | Bu projeyle farkı |
|---|---|---|
| Heroku | `git push` → buildpack → dyno | Release'ler sıralı; rollback eski release'i yeniden başlatır |
| Vercel / Netlify | Commit başına değişmez deployment + alias | Yönetilen ve kapalı; esin kaynağı bu model |
| Dokku / Coolify | Tek sunucuda Heroku benzeri | Docker üzerinde; commit başına kalıcı URL yok |
| Knative / Fly.io | Revision'lar, trafik bölme | Daha genel; Git entegrasyonu dışarıda |

paas, Vercel'in değişmez deployment + alias modelini Kubernetes nesnelerine birebir eşler:
**deployment = Deployment + Service + Ingress**, **alias = ayrı bir Ingress**.

## 4. Mimari

Ayrıntılı diyagramlar: [ARCHITECTURE.md](ARCHITECTURE.md).

Kontrol düzlemi tek bir Go ikilisidir. Ayrı mikroservisler yerine tek süreç seçildi:
bileşenler aynı veritabanı durumunu paylaşır, aralarında ağ çağrısı yoktur, dağıtım ve
hata ayıklama basitleşir. Ölçeklenmesi gereken tek parça olan build işi zaten ayrı bir
süreçte (buildkitd) çalışır.

Bir push'un yolu:

1. **Webhook:** HMAC-SHA256 imzası sabit zamanlı karşılaştırmayla doğrulanır; `(app, commit)`
   benzersiz olduğundan GitHub'ın tekrar teslimatı yeni iş üretmez.
2. **Kuyruk:** `UPDATE … WHERE id = (SELECT … FOR UPDATE SKIP LOCKED LIMIT 1)` ile iş
   alınır; worker'lar birbirini beklemez, bir iş iki kez alınmaz.
3. **Build:** depo commit SHA'sı ile çekilir (`git fetch --depth=1 <sha>`), proje tipi
   algılanır, gerekirse Dockerfile üretilir, BuildKit build edip registry'ye push'lar; imaj
   digest ile sabitlenir.
4. **Deploy:** uygulama namespace'i (kota, limit, ağ politikası), değişmez env Secret'ı,
   Deployment, Service ve Ingress oluşturulur; pod hazır olana kadar beklenir.
5. **Alias:** deployment `ready` olunca alias'lar veritabanında taşınır ve Ingress'lere
   senkronlanır.

## 5. Tasarım kararları

### 5.1 Değişmez deployment'lar ve alias'lar

Her commit kendi Kubernetes nesneleriyle (`d-<sha7>`) çalışır ve hiç güncellenmez. Production
ayrı bir Ingress'tir ve yalnızca backend'i değişir. Sonuçları:

- **Rollback anlık:** eski deployment zaten çalışıyor; değişen tek şey bir Ingress alanı.
  Ölçülen API gecikmesi p95 23 ms.
- **Her commit test edilebilir:** `https://<sha7>-<app>.<domain>` kalıcıdır.
- **Bedel kaynaktır:** her deployment bir pod tutar. Bu, uygulama başına `ResourceQuota` ve
  saklama politikasıyla sınırlandı (bkz. 5.7).

Alias'lar **yalnızca ileri gider**: geç biten eski bir commit'in build'i, daha yeni bir
deployment'ın production'ını ezemez (`ON CONFLICT … WHERE aliases.deployment_id < EXCLUDED.deployment_id`).

### 5.2 Kuyruk olarak PostgreSQL

Redis veya RabbitMQ yerine PostgreSQL seçildi:

- **İşlemsel tutarlılık:** iş, durum ve alias aynı veritabanında; "kuyruğa yazıldı ama
  veritabanına yazılmadı" türü tutarsızlıklar oluşamaz.
- **Daha az parça:** tek sunuculu hedefte ayrı bir broker işletme yükü getirirdi.
- **Yeterli kapasite:** ölçümde webhook kabulü p95 < 250 ms, 8 worker'da doğrusala yakın
  ölçeklenme (bölüm 8). Darboğaz kuyruk değil, build'in kendisi.

Canlı loglar için de aynı veritabanı kullanılır: her log satırı `NOTIFY` üretir, SSE
istemcileri `LISTEN` ile uyanır; bağlantı koparsa periyodik okumaya düşülür.

### 5.3 Veritabanı tek doğru kaynak; küme ona uzlaştırılır

Alias'ların nereyi gösterdiği yalnızca Postgres'te kararlaştırılır. Ingress'ler bu durumun bir
yansımasıdır ve üç yerden senkronlanır: deploy sonrası, rollback sonrası ve dakikada bir
çalışan uzlaştırma döngüsü. Senkron idempotenttir (aynı durum kümeye hiçbir yazma yapmaz) ve
uygulama başına sıraya girer; her senkron veritabanını kilit içinde okuduğu için geç biten
eski bir senkron yeni bir rollback'i geri alamaz. Yönlendirme katmanı hata verirse rollback
kaybolmaz: veritabanına kaydedilir, istemciye `502` döner, döngü uygular.

### 5.4 Build: SHA ile clone, algılama, üretilmiş Dockerfile

- Branch değil **commit SHA'sı** çekilir; kuyrukta beklerken gelen bir push build'i değiştiremez.
  Private repo token'ı git'e ortam değişkeniyle verilir; komut satırına ve loglara sızmaz.
- Repoda Dockerfile varsa o kullanılır; yoksa Node (npm / yarn / pnpm; sunucu ya da statik
  build), Go ve statik site için Dockerfile üretilir ve build logunda aynen gösterilir.
- Üretilen imajlar **sayısal kullanıcıyla** çalışır (node 1000, distroless 65532, nginx 101).
  İsimli `USER` ile kubelet `runAsNonRoot`'u doğrulayamaz; sayısal UID ile tüm uygulamalar
  root olmayan kullanıcıyla zorunlu çalıştırılabilir.
- Bağımlılık katmanları manifest'lerden önce kopyalanır ve cache mount'lar kullanılır.
- İmaj referansı digest'le sabitlenir (`app:sha@sha256:…`); etiket üzerine yazılsa bile
  çalışan imaj değişmez.
- Kümede BuildKit **rootless** çalışır; yerelde aynı Dockerfile Docker Desktop ile build edilir.

### 5.5 Deploy: güvenli varsayılanlar, hızlı başarısızlık

Pod'lar `runAsNonRoot`, `allowPrivilegeEscalation: false`, tüm Linux yetkileri düşürülmüş,
seccomp `RuntimeDefault`, servis hesabı token'ı bağlanmamış olarak çalışır. Uygulama
namespace'leri varsayılan olarak yalnızca Traefik'ten trafik kabul eder; uygulamalar
birbirine doğrudan erişemez.

Hazır olma beklenirken `CrashLoopBackOff`, `ImagePullBackOff` ya da kota aşımı görüldüğünde
zaman aşımı beklenmez; deployment hemen `failed` olur ve crash durumunda uygulamanın son log
satırları hata mesajına eklenir.

### 5.6 Standart Ingress (CRD değil)

Faz planında Traefik `IngressRoute` öngörülmüştü; uygulamada standart
`networking.k8s.io/v1 Ingress` seçildi. Traefik ikisini de destekler; standart Ingress CRD
gerektirmez, client-go ile tipli ve test edilebilirdir, başka bir ingress controller ile de
çalışır. TLS bölümünde `secretName` verilmez; Traefik varsayılan sertifika olarak cert-manager'ın
ürettiği wildcard sertifikayı sunar. Tüm host'lar tek etiketli olduğu için (`<sha7>-<app>.<domain>`)
tek bir `*.domain` sertifikası hepsini kapsar ve Let's Encrypt hız sınırlarına takılınmaz.

### 5.7 Yaşam döngüsü ve kurtarma

- **Saklama:** bir alias'ın gösterdiği deployment'a asla dokunulmaz; production branch'inin en
  yeni N deployment'ı rollback hedefi olarak tutulur; alias'ı kalmamış preview'ler TTL sonra
  `retired` olur ve nesneleri silinir. Rollback ile temizlik aynı anda olursa `FOR SHARE`
  kilidi rollback'i korur.
- **Branch silinmesi / PR kapanması:** preview alias'ı hemen kalkar.
- **Çökme sonrası kurtarma:** worker çalıştığı iş için heartbeat yazar. Heartbeat'i duran iş
  yeniden kuyruğa alınır (en fazla 2 deneme). Eski worker hâlâ yaşıyorsa heartbeat'i
  reddedilir ve işini iptal eder; böylece bir iş hiçbir zaman iki worker tarafından
  aynı anda yürütülmez.

Tüm arıza senaryoları ve onları doğrulayan testler: [FAILURE-SCENARIOS.md](FAILURE-SCENARIOS.md).

### 5.8 Bağımlılıklar

Go standart kütüphanesi, `lib/pq` ve Kubernetes için `client-go`. HTTP sunucusu, router,
GitHub istemcisi, oturum ve CSRF standart kütüphaneyle yazıldı. client-go bilinçli bir
istisnadır: Kubernetes API'sinin kimlik doğrulama, yeniden deneme ve tip güvenliğini elle
yazmak hem riskli hem gereksiz olurdu.

## 6. Güvenlik

| Tehdit | Önlem |
|---|---|
| Sahte webhook | HMAC-SHA256 imzası, sabit zamanlı karşılaştırma |
| API'ye yetkisiz erişim | Kişisel bearer token'lar (`paas_` + 256 bit, yalnızca SHA-256 özeti saklanır, süre ve iptal); admin token'ı sabit zamanlı karşılaştırma, GitHub girişi varken kapatılabilir |
| Ekipler arası erişim | Her API ucu ve sayfa çağıranın ekip rolünü denetler (viewer/member/owner); başka ekibin uygulaması 404 döner, isimler sızmaz; yeni uçlar tek sarmalayıcıyla korunur, sarmalanmamış uç da varsayılan rolle korunur |
| Yetkisiz GitHub hesabıyla giriş | OAuth yalnızca izin listesindeki kullanıcı/organizasyon üyelerine, admin'lere ve ekibe davet edilenlere açık; varsayılan kapalı |
| Başkasının GitHub App kurulumunu sahiplenme | Kurulum ancak taze bir kullanıcı token'ıyla (PKCE) `GET /user/installations` onu listeliyorsa ve token, akışı başlatan platform kullanıcısının hesabına aitse bir ekibe bağlanır; ekip ve kullanıcı 30 dakikalık imzalı state'te; kullanıcı ekipte member+ olmalı; başka ekibe bağlı kurulumu taşımak orada da member+ ister |
| OAuth akışında CSRF / kod çalınması | 10 dakikalık imzalı `state` çerezi (sabit zamanlı karşılaştırma), PKCE S256; GitHub erişim token'ı saklanmaz |
| Son owner'ın kaybı | Ekip satırı kilitlenerek üye değişikliği yapılır; son owner düşürülemez/çıkarılamaz |
| Web arayüzünde oturum çalınması / CSRF | Durumsuz, HMAC imzalı `HttpOnly` + `SameSite=Strict` çerez; oturuma bağlı CSRF token'ı; Origin kontrolü; açık yönlendirme engeli |
| XSS | `html/template` otomatik kaçışlama; istek başına nonce'lu sıkı CSP |
| Sırların sızması | Env değerleri API'den ve arayüzden asla okunmaz; deploy anında değişmez Secret; GitHub token'ı argv/log'a yazılmaz; altyapı sırları Terraform state'ine girmez |
| Kötü niyetli uygulama | Root olmayan pod, yetkiler düşürülmüş, seccomp; uygulamalar arası ağ yasak; kota ve limitler |
| Bulut kimlik bilgileri | IMDSv2 hop limit 1: instance rolüne yalnızca host ağı erişir, uygulama pod'ları erişemez |

## 7. Test ve doğrulama

- **79 test fonksiyonu**, ~3 750 satır test kodu.
- Veritabanı testleri gerçek PostgreSQL'e karşı çalışır; eşzamanlılık (8 worker × 50 iş),
  idempotentlik, alias'ın yalnızca ileri gitmesi, rollback/temizlik yarışı, çökme kurtarması.
- Kubernetes katmanı client-go fake clientset ile: nesnelerin doğruluğu, idempotentlik,
  crash loop / imaj / kota hatalarında hızlı başarısızlık.
- Gerçek build testi (`make test-build`): üç örnek uygulama gerçekten build edilir, registry'ye
  push'lanır, digest ile çalıştırılır ve HTTP yanıtı kontrol edilir.
- Uçtan uca duman testleri: imzalı webhook → build → dry-run deploy → alias; web arayüzünde
  giriş, canlı log akışı.
- Yerel gerçek küme (k3d): gerçek build, Kubernetes deploy, Traefik yönlendirmesi, trafik
  altında rollback, hata ve temizlik senaryoları (bölüm 8).
- Altyapı: `terraform validate`, kubeconform ile 19 Kubernetes kaynağının şema doğrulaması,
  tüm Mermaid diyagramlarının mermaid-cli ile doğrulanması.

Entegrasyon sırasında testler gerçek hatalar yakaladı: veritabanı ile uygulama saatleri
arasındaki kaymanın canlı log akışını erken kapatması, bir şablon tip hatası ve yeni `retired`
durumunun canlı log ve web arayüzü tarafından bitmiş sayılmaması. Gerçek küme koşusu da iki
sorun gösterdi: crash mesajının hatanın nedenini değil stack trace'in sonunu göstermesi ve TLS
kapalıyken adreslerin `https://` yazılması.

## 8. Ölçümler

Ayrıntılar ve yeniden üretme komutları: [MEASUREMENTS.md](MEASUREMENTS.md).

| Ölçüm | Sonuç |
|---|---|
| 40 deploy, 1 → 8 worker | 64.5 s → 9.5 s (6.8×), 0 kayıp / 0 tekrar |
| Webhook kabul süresi (40 eşzamanlı push) | p95 112–229 ms |
| Rollback API'si | p50 7 ms, p95 23 ms |
| Build, Go örneği | soğuk 19.4 s → kod değişikliği 4.3 s |
| Build, Node örneği | soğuk 8.1 s → kod değişikliği 2.6 s |
| İmaj boyutu | Go 3 MB, statik 20 MB, Node 61 MB |
| Push → ready, gerçek küme (k3d), kod değişikliği | 8.9 s |
| Rollback sırasında trafik, gerçek küme | 1 000 istek, 0 hata |

## 9. Sınırlar ve açık konular

Açıkça belirtilmesi gerekenler:

- **Yerel gerçek kümede doğrulandı, AWS'de henüz değil.** k3d (k3s v1.35, Traefik, NetworkPolicy)
  üzerinde uçtan uca çalıştırıldı: deploy, üç tür adres, trafik altında kesintisiz rollback
  (1 000 istek, 0 hata), bozuk commit'te hızlı başarısızlık, temizlik, branch silme, pod logları
  ([MEASUREMENTS.md §4](MEASUREMENTS.md#4-gerçek-küme-k3d-yerel)). k3s ağ politikasının readiness
  probe'larına izin verdiği ve Traefik'in standart Ingress'leri sunduğu böylece doğrulandı.
  Terraform yapılandırması doğrulandı ama `apply` edilmedi; AWS'de doğrulanacaklar: TLS ile
  wildcard sertifikanın varsayılan olarak uygulanması, ECR'a ilk push'ta repo oluşturma
  izinleri, kubelet ECR kimlik bilgisi sağlayıcısı, arm64 build süreleri.
- **Ölçümler dizüstü bilgisayarda alındı;** hedef sunucuda (arm64, 2 vCPU) tekrar alınmalı.
  Kubernetes'teki pod hazır olma süresi ve uygulama trafiği kapasitesi henüz ölçülmedi.
- **Tek düğüm:** Postgres ve build cache düğümün diskinde; düğüm kaybı veri kaybıdır.
- **Tek kontrol düzlemi kopyası varsayılır;** çok kopyada yönlendirme senkronları arasında
  kısa yarışlar olabilir (uzlaştırma döngüsü düzeltir).
- **Env değerleri veritabanında şifreli (Faz 10):** AES-256-GCM, değer başına rastgele nonce,
  uygulama + değişken adına bağlı; anahtar `PAAS_ENV_KEY` ile verilir, rotasyon desteklenir. Anahtar
  ortam değişkeni olarak (Kubernetes Secret) durur, KMS ile sarılmaz; anahtarı ele geçiren veritabanı
  dökümünü de çözebilir. Anahtar kaybı şifreli değerlerin kaybıdır.
- **Fork'lardan açılan PR'lar** preview yorumu almaz.
- **Kullanıcı adı tabanlı izin listeleri:** `PAAS_ADMIN_GITHUB_LOGINS` ve `PAAS_ALLOWED_GITHUB_USERS`
  GitHub kullanıcı adlarıyla eşleşir; GitHub'da bırakılan bir ad başkası tarafından alınabilir.
  Organizasyon üyeliği veya ekip daveti (hesap numarasına bağlı) daha sağlamdır.
- **GitHub App kurulumu bir ekibe bağlanır (Faz 15):** aynı kurulumun repoları iki ekip arasında
  paylaştırılamaz; bir organizasyonun repoları farklı ekiplere dağıtılacaksa kurulum tek ekipte kalır
  ve import o ekipte yapılır. Bağlanmamış bir kurulumun repoları listelenmez.
- **Durumsuz oturumlar tek tek iptal edilemez;** kullanıcının ekipten çıkarılması anında etkilidir
  (rol her istekte okunur), ama oturumun kendisi süresi dolana veya anahtar değişene kadar geçerlidir.

## 10. Gelecek çalışmalar

1. Gerçek küme üzerinde kurulum, uçtan uca doğrulama ve ölçümlerin tekrarı.
2. Sıfıra ölçekleme (KEDA HTTP add-on): kullanılmayan preview'ler kaynak tüketmez.
3. Env şifreleme anahtarının KMS ile sarılması (zarf şifreleme) ve ekip bazlı yetkilendirme.
4. Özel alan adları (HTTP-01 ya da DNS doğrulamalı sertifikalar).
5. Yönetilen veritabanı (RDS) ve çok düğümlü küme.
6. Dockerfile frontend'inin digest'e sabitlenmesi (ılık build'lerdeki ~2 s'lik sabit maliyet).
7. Daha fazla dil (Python, Ruby, Java) ve framework'e özgü algılama.

## Ekler

- Kurulum ve yerel geliştirme: [README.md](../README.md)
- Altyapı: [infra/README.md](../infra/README.md)
- Faz planı: [PHASES.md](PHASES.md)
- Demo senaryosu: [DEMO.md](DEMO.md)
- Yük testleri: [loadtest/README.md](../loadtest/README.md)
