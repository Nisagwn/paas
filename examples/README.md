# Örnek uygulamalar

paas'ın otomatik algıladığı proje tipleri. Hiçbirinde Dockerfile yok;
platform onu kendisi üretir. Sözleşme: uygulama `$PORT` (8080) üzerinden dinler.
Üretilen imajların hepsi sayısal, root olmayan bir kullanıcıyla çalışır.

| Dizin | Algılanan tip | Üretilen imaj | Kullanıcı |
|---|---|---|---|
| `node-hello/` | Node, `start` script'i | `node:22-alpine`, `npm start` | `1000:1000` |
| `go-hello/` | Go, kökte `package main` | distroless/static, tek binary | `65532:65532` |
| `static-site/` | `index.html` | nginx-unprivileged | `101` |
| `python-hello/` | Python, `requirements.txt` + FastAPI | `python:3.12-slim`, venv, `uvicorn main:app` | `1000:1000` |
| `ruby-hello/` | Ruby, `Gemfile.lock` + `config.ru` (Rack) | `ruby:3.3-slim`, `bundle exec puma` | `1000:1000` |
| `java-hello/` | Java, `pom.xml` (Maven) | `eclipse-temurin:21-jre`, `java -jar` | `1000:1000` |
| `nextjs-hello/` | Next.js (`next` bağımlılığı), `output: 'standalone'` | `node:22-alpine`, yalnızca `.next/standalone` + `.next/static`, `node server.js` | `1000:1000` |
| `websocket-chat/` | Node, `start` script'i; bağımlılıksız WebSocket sunucusu (`/ws`) | `node:22-alpine`, `npm start` | `1000:1000` |
| `worker-queue/` | Node + `paas.yaml`: web, worker ve 10 dakikada bir cron | `node:22-alpine`; her süreç kendi komutuyla | `1000:1000` |
| `vite-hello/` | Vite (`vite` bağımlılığı), `vite build` | nginx-unprivileged, `dist/` | `101` |

Her birini ayrı bir GitHub reposuna koyup paas'a bağlayabilirsin.

`worker-queue` süreç tiplerini (Faz 20) gösterir: worker ve cron web sürecine uygulamanın genel adresinden ulaşır,
bu yüzden deploy'dan sonra `paas env set worker-queue QUEUE_URL=https://worker-queue.<domain>` (isteğe bağlı
`QUEUE_TOKEN`) ver ve yeniden deploy et. `paas ps worker-queue`, `paas ps scale worker-queue worker=2` ve
`paas cron run worker-queue report` ile dene. `websocket-chat` ise uyutulmuş bir deploy'u ilk WebSocket
bağlantısıyla uyandırır.
`make test-build` hepsini gerçekten build edip çalıştırır, imaj kullanıcısının sayısal
olduğunu ve HTTP yanıtını kontrol eder.
