# Yük testleri (k6)

İki senaryo var:

| Script | Neyi ölçer |
|---|---|
| `webhook-burst.js` | Aynı anda gelen çok sayıda imzalı push: webhook'un kabul süresi (`paas_enqueue_ms`) ve push'tan `ready`'ye kadar geçen süre (`paas_time_to_ready_ms`) |
| `app-traffic.js` | Deploy edilmiş bir uygulamaya sabit hızda istek: DNS → Traefik (TLS) → Service → pod yolunun gecikmesi ve hata oranı |

## Çalıştırma

```bash
# Push patlaması: 50 push, 10 eşzamanlı istemci
k6 run -e PAAS_URL=http://localhost:8080 \
       -e TOKEN=$PAAS_API_TOKEN -e SECRET=$PAAS_GITHUB_WEBHOOK_SECRET \
       -e PUSHES=50 -e VUS=10 loadtest/webhook-burst.js

# Uygulama trafiği: saniyede 200 istek, 60 saniye
k6 run -e URL=https://blog.paas.example.com/ -e RATE=200 -e DURATION=60s loadtest/app-traffic.js
```

`webhook-burst.js` gerçek bir build yapılan ortamda `REPO` olarak gerçekten var olan bir repo
ister (rastgele SHA'lar clone edilemez); bu yüzden kontrol düzleminin kendisini ölçmek için
`PAAS_BUILDER=dryrun` ile, uçtan uca build süresini ölçmek için ise gerçek commit'lerle kullanılır.

Rollback sırasında kesinti olmadığını göstermek için `app-traffic.js` çalışırken
`POST /api/apps/{name}/rollback` çağrılır; `http_req_failed` sıfır kalmalıdır.

## Kaydedilecekler

- `paas_enqueue_ms` p50 / p95: webhook'un kuyruğa yazma süresi
- `paas_time_to_ready_ms` p50 / p95: worker sayısı (`PAAS_WORKERS`) ile nasıl değiştiği
- `http_req_duration` p95 ve `http_req_failed`: uygulama trafiğinde, normalde ve rollback anında
- Kontrol düzleminin CPU/RAM kullanımı (`kubectl top pod -n paas`)

## Örnek sonuç (yerel, dry-run)

Windows 11 dizüstü, Postgres Docker'da, `PAAS_BUILDER=dryrun`, `PAAS_DEPLOYER=dryrun`
(her deploy 4 × 500 ms taklit aşama), `PAAS_WORKERS=4`, 30 push / 10 eşzamanlı istemci:

| Metrik | avg | med | p95 | max |
|---|---|---|---|---|
| `paas_enqueue_ms` | 12.5 ms | 6.5 ms | 32.6 ms | 36.8 ms |
| `paas_time_to_ready_ms` | 3.6 s | 3.56 s | 4.84 s | 5.08 s |

275 HTTP isteği, %0 hata. Tek bir deploy'un en kısa süresi 2.02 s (taklit aşamaların toplamı);
aradaki fark kuyrukta bekleme süresidir: 30 iş 4 worker'a paylaşılır.
