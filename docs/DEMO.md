# Demo senaryosu

Canlı demo ve demo videosu için adım adım akış. Süre: ~10 dakika.
Her adımda **ne gösterilir** ve **ne söylenir** ayrı yazılmıştır.

## Hazırlık (demodan önce, bir kez)

1. Platform ayakta: `terraform apply` tamamlanmış, `https://<domain>` web arayüzü açılıyor
   ([infra/README.md](../infra/README.md)).
2. GitHub'da bir demo reposu, örneğin `examples/node-hello`'nun kopyası: `nisagwn/demo-blog`.
   Webhook ekli: `https://<domain>/webhooks/github`, **Pushes** + **Pull requests**.
3. Repo platformda tanımlı: web arayüzünde *New app* → ad `blog`, repo `nisagwn/demo-blog`.
4. En az bir production deploy'u var (rollback adımı için eski bir hedef gerekir).
5. Açık sekmeler: web arayüzü, demo reposu, `https://blog.<domain>`, terminal.
6. Yedek plan: ağ sorunu olursa yerel ortam (`make run`, `PAAS_BUILDER=docker`) ve kayıtlı video.

## Akış

| # | Gösterilen | Söylenen |
|---|---|---|
| 1 | Web arayüzünde `blog` uygulaması: production alias'ı, deploy geçmişi | "Her commit ayrı, değişmez bir deploy. Production yalnızca bir takma ad." |
| 2 | Editörde `server.js`'teki mesajı değiştir, `git push` (main) | "Tek yaptığım push. Gerisi platformda." |
| 3 | Deploy sayfası: canlı log akıyor (clone → algılama → üretilen Dockerfile → BuildKit → Kubernetes) | "Repoda Dockerfile yok; platform Node projesini algılayıp üretti. Loglar Postgres LISTEN/NOTIFY ile canlı geliyor." |
| 4 | GitHub'da commit: `paas/deploy` status'u pending → success | "GitHub entegrasyonu: commit'in yanında deploy durumu ve linki." |
| 5 | `https://<sha7>-blog.<domain>` ve `https://blog.<domain>` açılır, yeni mesaj görünür | "Bu commit'in kalıcı URL'si; production alias'ı da ona taşındı. Wildcard sertifika: her host otomatik HTTPS." |
| 6 | Yeni branch `feature/dark-mode`, değişiklik, push, PR aç | "Branch'ler preview alır." |
| 7 | PR'da "✅ Preview hazır" yorumu; `https://feature-dark-mode-blog.<domain>` | "Gözden geçiren, PR'dan çıkmadan değişikliği canlı görüyor." |
| 8 | **Bozuk bir commit** push'la (ör. `server.js`'te sözdizimi hatası) | "Hatalı bir deploy ne olur?" |
| 9 | Deploy sayfası: `CrashLoopBackOff` → ~10–20 saniyede `failed` (zaman aşımı beklenmez), hata mesajında uygulamanın son log satırları; production hâlâ eski sürümde | "Alias yalnızca hazır deploy'lara taşınır; kullanıcı hiçbir şey fark etmedi." |
| 10 | Terminalde `k6 run loadtest/app-traffic.js` (production'a sürekli istek), arayüzde eski deploy'da **Rollback to this** | "Rollback build değil, yalnızca bir Ingress'in backend'ini çevirmek; birkaç milisaniye." |
| 11 | k6 özeti: `http_req_failed 0.00%` | "Rollback boyunca tek bir istek bile başarısız olmadı." |
| 12 | PR'ı birleştirmeden kapat | "Kapanan PR'ın preview'i otomatik kalkar; kaynaklar geri kazanılır." |
| 13 | [MEASUREMENTS.md](MEASUREMENTS.md) tabloları | Ölçümler: kuyruk ölçeklenmesi, build süreleri, rollback gecikmesi. |

## Komutlar

```bash
# 2 — değişiklik ve push
sed -i 's/hello from node/hello from the demo/' server.js && git commit -am "demo: new message" && git push

# 6 — preview
git switch -c feature/dark-mode && echo "// dark" >> server.js && git commit -am "dark mode" && git push -u origin HEAD
gh pr create --fill

# 8 — bozuk commit
echo "this is not javascript" >> server.js && git commit -am "broken" && git push

# 10 — rollback sırasında trafik
k6 run -e URL=https://blog.<domain>/ -e RATE=50 -e DURATION=45s loadtest/app-traffic.js

# 12 — PR'ı kapat
gh pr close feature/dark-mode
```

## Video için notlar

- Çözünürlük 1920×1080, tarayıcı yakınlaştırması %125, terminal yazı tipi büyük.
- Build adımında bekleme olursa (ilk build ~10–20 s) video kurgusunda hızlandırılır ve
  ekranda "×4" gibi bir not gösterilir.
- Her bölüm başında kısa bir başlık kartı: *Push → Deploy*, *Preview*, *Hatalı deploy*,
  *Rollback*, *Temizlik*.
