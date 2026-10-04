# Ölçümler

Tüm sayılar bu repodaki script'lerle üretildi ve yeniden üretilebilir. Ham sonuçlar
[`docs/measurements/`](measurements/) altında.

**Ortam:** Windows 11 dizüstü (x86-64), Docker Desktop 28.5, PostgreSQL 16 (Docker), Go 1.26, k6.
Bunlar yerel ölçümlerdir; hedef sunucuda (EC2 t4g.medium, arm64) tekrar alınmaları gerekir
(bkz. [Sınırlar](#sınırlar)).

## 1. Kuyruk ölçeklenmesi: worker sayısı

40 push aynı anda gelir (40 eşzamanlı istemci); her deploy dry-run pipeline ile ~1.5 s sürer
(3 × 500 ms taklit aşama). Ölçülen: webhook'un kabul süresi, push'tan `ready`'ye geçen süre ve
40 deploy'un tamamının bitme süresi.

```bash
scripts/measure-workers.sh 40 1 2 4 8
```

| Worker | Toplam süre | Hızlanma | Push → ready (medyan) | Push → ready (p95) | Webhook kabul (p95) | Başarısız |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 64.5 s | 1.0× | 32.8 s | 60.9 s | 229 ms | 0 / 40 |
| 2 | 32.8 s | 2.0× | 17.0 s | 30.6 s | 157 ms | 0 / 40 |
| 4 | 17.1 s | 3.8× | 9.3 s | 16.4 s | 112 ms | 0 / 40 |
| 8 | 9.5 s | 6.8× | 5.3 s | 8.6 s | 184 ms | 0 / 40 |

```mermaid
xychart-beta
    title "40 deploy'un bitme süresi (s)"
    x-axis "Worker sayısı" [1, 2, 4, 8]
    y-axis "Saniye" 0 --> 70
    bar [64.5, 32.8, 17.1, 9.5]
```

**Yorum:**
- Postgres kuyruğu (`FOR UPDATE SKIP LOCKED`) 8 worker'a kadar neredeyse doğrusal ölçekleniyor;
  160 deploy'un hiçbiri iki kez alınmadı ya da kaybolmadı (bu ayrıca
  `TestClaimNextIsExclusive` ile 8 worker × 50 işte doğrulanıyor).
- Webhook kabul süresi worker sayısından bağımsız olarak 250 ms'nin altında kalıyor: GitHub'ın
  10 saniyelik webhook zaman aşımının çok altında. Medyanın (~90 ms) 10 istemcili koşudaki
  ~6 ms'ye göre yüksek olması, aynı anda 40 istemcinin durumu her 500 ms'de sorgulamasından.
- Gerçek ortamda darboğaz worker değil build'dir: 2 vCPU'lu bir düğümde aynı anda 2–3 build'den
  fazlası birbirini yavaşlatır. `PAAS_WORKERS` bu yüzden varsayılan olarak 2.

## 2. Build süresi ve imaj boyutu

Platformun ürettiği Dockerfile ile, `examples/` altındaki üç uygulama. **Soğuk:** katman cache'i
yok. **Ilık:** hiçbir şey değişmedi. **Kod değişikliği:** bir kaynak dosya değişti, bağımlılık
katmanları cache'te.

```bash
scripts/measure-builds.sh
```

| Uygulama | Algılanan tip | Soğuk | Ilık | Kod değişikliği | İmaj |
|---|---|---:|---:|---:|---:|
| `node-hello` | Node, `start` script'i | 8.1 s | 2.6 s | 2.6 s | 61 MB |
| `go-hello` | Go, distroless | 19.4 s | 2.3 s | 4.3 s | 3 MB |
| `static-site` | nginx-unprivileged | 3.2 s | 2.2 s | 2.3 s | 20 MB |

**Yorum:**
- Cache mount'lar (`/root/.npm`, `/go/pkg/mod`, `/root/.cache/go-build`) sayesinde Go'da kod
  değişikliği sonrası build, soğuk build'in %22'si kadar sürüyor.
- Bu tablo Faz 14'ten önce, `# syntax=docker/dockerfile:1` satırıyla alındı. Satır her build'de
  frontend imajını registry'de sorgular; kaldırılınca ılık build ~0.5–1.4 s kısalır (bölüm 7).
- Go imajı, distroless + statik ikili sayesinde 3 MB (Docker'ın raporladığı boyut).
  Küçük imaj, yeni bir düğümde ilk çekme süresini doğrudan kısaltır.
- Uçtan uca bir push → `ready` örneği (Docker builder, dry-run deploy, `node-hello`):
  clone 0.7 s, build + push 3.8 s, toplam 5.3 s.

## 3. Rollback gecikmesi

Production iki hazır deploy arasında 100 kez gidip gelir; API çağrısının toplam süresi ölçülür.

```bash
scripts/measure-rollback.sh 100
```

| n | p50 | p95 | max |
|---:|---:|---:|---:|
| 100 | 7.1 ms | 23.1 ms | 33.4 ms |

Bu, kontrol düzleminin payıdır (veritabanı işlemi + alias senkronu). Kubernetes'te buna
yalnızca tek bir Ingress'in güncellenmesi eklenir: yeniden build yok, pod yeniden başlamıyor,
çünkü eski deploy zaten çalışıyor. Rollback sırasında kesinti olmadığı
`loadtest/app-traffic.js` ile gerçek kümede doğrulanmalıdır (bkz. [DEMO.md](DEMO.md)).

## 4. Gerçek küme (k3d, yerel)

k3s v1.35 (k3d), Traefik, NetworkPolicy etkin; kontrol düzlemi `PAAS_BUILDER=docker`,
`PAAS_DEPLOYER=kubernetes` ile. Kurulum: `scripts/k3d-up.sh`.

| Senaryo | Sonuç |
|---|---|
| İlk deploy (cache yok), push → `ready` | 20.4 s |
| Kod değişikliği, push → `ready` | 8.9 s (clone 0.6 s, build + push 3.3 s, rollout 4 s) |
| Üç adres Traefik üzerinden (`<sha7>-blog`, `blog`, `main-blog`) | hepsi 200 |
| Rollback API'si (Ingress güncellemesi dahil) | 66–68 ms |
| **Rollback sırasında trafik** (50 istek/s, 20 s) | **1 000 istek, 0 hata**, p50 2.4 ms, p95 4.6 ms |
| Bozuk commit (Node sözdizimi hatası) | build dahil 12.4 s'de `failed`, production etkilenmedi |
| Başarısız deploy'un nesneleri | temizlik döngüsü sildi; Ingress sahiplik ilişkisiyle birlikte gitti |
| Branch silme webhook'u | preview alias'ı kalktı (404), deploy `retired`, nesneleri silindi |
| Pod logları (`runtime-logs`) | çalışıyor |

Bu koşu iki hata buldu ve düzeltildi: crash mesajı yalnızca stack trace'in sonunu gösteriyordu
(artık hatayı adlandıran satır başta), TLS kapalıyken adresler `https://` olarak yazılıyordu.

## 5. Test kapsamı

| | |
|---|---|
| Go kaynak kodu | ~6 600 satır (testler hariç) |
| Test kodu | ~3 750 satır, 79 test fonksiyonu |
| Veritabanı testleri | gerçek PostgreSQL'e karşı (`make test`) |
| Kubernetes testleri | client-go fake clientset |
| Gerçek build testleri | `make test-build`: üç örnek gerçekten build edilir, çalıştırılır, HTTP yanıtı kontrol edilir |

## 6. Sıfıra ölçekleme (k3d)

Bölüm 4'teki küme; kontrol düzlemi host'ta, `PAAS_SCALE_TO_ZERO_AFTER=1m`,
`PAAS_SCALE_INTERVAL=10s`, `PAAS_ACTIVATOR_UPSTREAM=http://127.0.0.1:80` (istek uyandırmadan
sonra Traefik üzerinden yeniden gönderilir). Uygulama `examples/node-hello`; bir production
(`main`) ve bir preview (`feature`) deploy'u. Gecikme `curl` ile, istemcinin gördüğü toplam süre.

| Senaryo | Sonuç |
|---|---|
| Son istekten sonra uyuma | 60–81 s (1 dk + en fazla bir kontrol aralığı); production izin yokken uyanık kaldı |
| Soğuk istek, deploy adresi (`<sha7>-s0-blog`) | 5.35 s, 4.52 s, 4.08 s — hepsi 200 |
| Soğuk istek, preview alias'ı (`feature-s0-blog`) | 7.47 s, 5.17 s — 200 |
| Soğuk istek, production (izin açıkken) | 4.21 s — 200 |
| Uyandırma süresi (aktivatör logu: replicas=1 → Deployment hazır → Service geri) | 3.8–4.9 s |
| Uyandırmadan hemen sonraki istek | 0.3–0.95 s (Traefik yapılandırmayı güncelleyene kadar aktivatör üzerinden) |
| Sıcak istek | 0.06–0.3 s |
| 10 eşzamanlı soğuk istek (biri gövdeli POST) | 10/10 × 200, 4.5–5.2 s; tek bir uyandırma |
| Production izni geri alındı | ölçekleyici uyuyan production'ı bir sonraki turda uyandırdı (4.1 s) |

Soğuk başlangıcın çoğu pod'un başlaması ve TCP readiness probe'udur (2 s aralık); kontrol
düzleminin payı (Ingress araması, ölçekleme, Service geçişi, isteğin iletilmesi) yaklaşık
0.4 s. Bu koşu bir hata buldu ve düzeltildi: Service selector'ı kaldırılınca Kubernetes eski
`Endpoints` nesnesini ve EndpointSlice'ı silmiyor, yansıtma denetleyicisi de eski pod adresini
yeni bir slice'a kopyalıyor; uyutma artık bunları siliyor, aksi halde Traefik isteklerin bir
kısmını sonlanmış pod'a gönderirdi.

## 7. Daha hızlı ılık build (Faz 14)

Üretilen Dockerfile'lar artık `# syntax=docker/dockerfile:1` satırıyla başlamıyor; BuildKit'in
yerleşik frontend'i cache mount'ları (`RUN --mount=type=cache`) zaten destekliyor. Ölçüm: her örnek
için aynı Dockerfile, satırlı ve satırsız, cache dolu, 7 koşu **dönüşümlü** (iki taraf aynı makine
yüküyle karşılaşsın diye); medyan.

```bash
scripts/measure-frontend.sh 7
```

| Uygulama | Satırla | Satırsız | Kazanç |
|---|---:|---:|---:|
| `node-hello` | 4.49 s | 3.12 s | −1.37 s (%31) |
| `go-hello` | 3.07 s | 2.53 s | −0.54 s (%18) |
| `static-site` | 2.72 s | 2.04 s | −0.69 s (%25) |
| `python-hello` | 2.88 s | 1.95 s | −0.94 s (%32) |
| `ruby-hello` | 2.87 s | 2.11 s | −0.76 s (%27) |
| `java-hello` | 2.79 s | 1.85 s | −0.94 s (%34) |

**Yorum:**
- Kazanç, build logundaki iki adımın düşmesinden gelir: `resolve image config for
  docker.io/docker/dockerfile:1` (registry'ye bir ağ gidiş-dönüşü) ve frontend imajının yüklenmesi.
  Bu yüzden kazanç ağ gecikmesiyle büyür; registry'ye uzak bir sunucuda daha belirgin olması beklenir.
- Kod değişikliğinden sonraki build'ler de aynı sabit maliyeti taşıdığından, push → `ready` süresi
  de aynı miktarda kısalır.
- Ham sonuçlar: [`docs/measurements/frontend.tsv`](measurements/frontend.tsv).

## Sınırlar

- Ölçümler dizüstü bilgisayarda alındı; hedef EC2 (arm64, 2 vCPU, 4 GiB) üzerinde build süreleri
  farklı olacaktır.
- Kuyruk ölçeklenmesi dry-run pipeline ile ölçüldü: kontrol düzleminin kendisini ölçer, gerçek
  build/deploy maliyetini değil.
- Gerçek küme ölçümleri yerel k3d üzerinde alındı (bölüm 4); hedef sunucuda (arm64, TLS,
  ECR) tekrar alınmalı. Uygulama trafiğinin üst sınırı (istek/saniye) henüz ölçülmedi.
