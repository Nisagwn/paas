# Örnek uygulamalar

minipaas'ın otomatik algıladığı üç proje tipi. Hiçbirinde Dockerfile yok;
platform onu kendisi üretir. Sözleşme: uygulama `$PORT` (8080) üzerinden dinler.

| Dizin | Algılanan tip | Üretilen imaj |
|---|---|---|
| `node-hello/` | Node, `start` script'i | `node:22-alpine`, `npm start` |
| `go-hello/` | Go, kökte `package main` | distroless/static, tek binary |
| `static-site/` | `index.html` | nginx-unprivileged |

Her birini ayrı bir GitHub reposuna koyup minipaas'a bağlayabilirsin.
`make test-build` hepsini gerçekten build edip çalıştırır ve HTTP yanıtını kontrol eder.
