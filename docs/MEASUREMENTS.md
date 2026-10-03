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
- Ilık build'lerin ~2.2 s'lik tabanı büyük ölçüde sabit maliyettir: üretilen Dockerfile'lardaki
  `# syntax=docker/dockerfile:1` satırı her build'de frontend imajını registry'de sorgular.
  Frontend'i sabit bir digest'e bağlamak bu süreyi kısaltabilir (sonraki iş).
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

## 4. Test kapsamı

| | |
|---|---|
| Go kaynak kodu | ~6 600 satır (testler hariç) |
| Test kodu | ~3 750 satır, 79 test fonksiyonu |
| Veritabanı testleri | gerçek PostgreSQL'e karşı (`make test`) |
| Kubernetes testleri | client-go fake clientset |
| Gerçek build testleri | `make test-build`: üç örnek gerçekten build edilir, çalıştırılır, HTTP yanıtı kontrol edilir |

## Sınırlar

- Ölçümler dizüstü bilgisayarda alındı; hedef EC2 (arm64, 2 vCPU, 4 GiB) üzerinde build süreleri
  farklı olacaktır.
- Kuyruk ölçeklenmesi dry-run pipeline ile ölçüldü: kontrol düzleminin kendisini ölçer, gerçek
  build/deploy maliyetini değil.
- Kubernetes deploy süresi (pod'un hazır olması), Ingress yayılma süresi ve uygulama trafiğinin
  istek/saniye kapasitesi gerçek kümede ölçülmelidir: `loadtest/app-traffic.js`.
