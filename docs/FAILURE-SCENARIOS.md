# Arıza senaryoları

Her senaryo için: ne olur, sistem nasıl davranır, hangi test bunu gösterir.
Genel ilke: **Postgres tek doğru kaynaktır**; küme ve GitHub ona göre uzlaştırılır.
Bir yan sistemin (router, GitHub) hatası deployment'ı düşürmez; geri dönüşü olmayan
bir hata (build hatası, crash loop) ise zaman aşımını beklemeden hemen `failed` olur.

| # | Senaryo | Davranış | Test |
|---|---|---|---|
| 1 | **Worker süreci çöker** (OOM, node kaybı, deploy sırasında yeniden başlatma) | Worker çalıştığı deployment için heartbeat yazar. `PAAS_WORKER_STALE_AFTER` (2 dk) boyunca heartbeat gelmezse deployment yeniden kuyruğa alınır; `attempts` sayacı 2'ye ulaşınca `failed` ("worker restarted"). Eski worker hâlâ yaşıyorsa heartbeat'i reddedilir ve işi iptal edilir: aynı deployment'ı iki worker birden yürütmez. | `worker/recovery_test.go`: `TestRecoverOrphanedDeployment`, `TestRecoverGivesUpAfterMaxAttempts`; `store/lifecycle_test.go`: `TestRecoverStale` |
| 2 | **Build hatası** (derleme hatası, eksik dosya, desteklenmeyen proje) | `failed`, hata mesajı ve build çıktısı logda. Alias almaz, rollback hedefi olamaz (409). | `api/flow_test.go`: `TestFailedBuildCannotReceiveTraffic`; `build/build_test.go`: `TestBuilderFailures` |
| 3 | **Build/deploy asılı kalır** | `PAAS_DEPLOY_TIMEOUT` (15 dk) dolunca `failed`; satır `building`'de kalmaz. | `worker/worker_test.go`: `TestTimeoutMarksFailed` |
| 4 | **Pod crash loop** | `CrashLoopBackOff` görülür görülmez `failed`; hatada çıkış kodu ve son log satırları. Eski production etkilenmez (alias yalnızca `ready`'ye taşınır). | `deploy/kubernetes_test.go`: `TestDeployCrashLoopFailsFast` |
| 5 | **İmaj çekilemiyor** | `ImagePullBackOff` / `InvalidImageName` → hemen `failed`. | `TestDeployImagePullFailsFast` |
| 6 | **Kota doldu** | `exceeded quota` → hemen `failed`. Temizlik döngüsü eski deploy'ları emekliye ayırdıkça yer açılır. | `TestDeployQuotaExceeded`; `cleanup/cleanup_test.go`: `TestCollectApp` |
| 7 | **İmaj root olarak çalışıyor / isimli USER** | Kubelet reddeder; hata mesajı sayısal `USER` kullanmayı önerir. | `TestDeployNonRootHint` |
| 8 | **Yönlendirme (Ingress API) hatası** | Deploy sonrası: deployment yine `ready` (kendi URL'si çalışır), logda uyarı. Rollback: veritabanına kaydedilir, API `502` döner. Her iki durumda dakikalık uzlaştırma düzeltir. | `api/routing_test.go`: `TestDeployRouterFailureIsWarning`, `TestRollbackRouterFailure` |
| 9 | **Eski commit'in build'i geç biter** | Alias yalnızca ileri gider; daha yeni deploy'un production'ını ezmez. | `store/store_test.go`: `TestAliasOnlyMovesForward` |
| 10 | **Branch, build sürerken silinir** | Deployment biter ama `retired` olur, alias almaz; nesneleri temizlenir. | `store/lifecycle_test.go`: `TestDeleteBranch`; `api/flow_test.go`: `TestBranchDeletedWebhook` |
| 11 | **Rollback ile temizlik aynı anda** | Rollback satırı `FOR SHARE` ile kilitler; temizlik alias'ı görür ve deployment'ı tutar. Emekli bir deployment'a rollback `409`. | `TestRetirementPolicy`, `TestCollectApp` |
| 12 | **Küme nesneleri silinemedi** (API geçici hata) | Deployment `retired` olur ama `cleaned_at` boş kalır; bir sonraki temizlikte tekrar denenir. | `TestCollectApp` |
| 13 | **GitHub API erişilemez / yavaş** | Status ve PR yorumu atlanır, logda uyarı; her çağrının kendi zaman aşımı var, deployment etkilenmez. | `worker/notify_test.go`: `TestNotifierErrorsAreLogged` |
| 14 | **Canlı log dinleyicisi kopar** (Postgres bağlantısı) | SSE akışı periyodik okumaya düşer, satır kaybolmaz; dinleyici yeniden bağlanınca abonelerini uyandırır. | `store/notify_test.go`: `TestHubNotifications`; `api/stream_test.go`: `TestStreamLogsPollingAndAuth` |
| 15 | **Veritabanı erişilemez** | `/healthz` 503 döner (Kubernetes pod'u trafikten çıkarır). Worker hatayı loglar ve bir sonraki yoklamada tekrar dener. Webhook 500 döner; GitHub teslimatı başarısız gösterir ve "Redeliver" ile yeniden gönderilebilir. Aynı commit'in tekrar gelmesi idempotenttir. | `api/flow_test.go`: tekrar teslimat (`TestDeployFlow`) |
| 16 | **Aynı webhook iki kez gelir** | `(app, commit)` benzersiz: ikinci teslimat mevcut deployment'ı döner, yeni build başlamaz. | `TestDeployFlow`, `store/store_test.go`: `TestEnqueueIsIdempotent` |

## Elle doğrulama (gerçek kümede)

```bash
# 1. Worker çökmesi: deploy sürerken kontrol düzlemi pod'unu öldür
kubectl -n paas delete pod -l app.kubernetes.io/name=paas --wait=false
# ~2 dk sonra deployment yeniden kuyruğa alınır ve tamamlanır (logda "queued again")

# 4. Crash loop: başlarken çıkan bir uygulama push'la
#    deployment birkaç saniye içinde "failed", hata mesajında son log satırları

# 8. Rollback'te kesinti olmaması
k6 run -e URL=https://blog.<domain>/ -e RATE=100 -e DURATION=60s loadtest/app-traffic.js &
curl -H "Authorization: Bearer $TOKEN" -d '{"deployment_id":<eski>}' https://<domain>/api/apps/blog/rollback
# http_req_failed = 0 kalmalı
```
