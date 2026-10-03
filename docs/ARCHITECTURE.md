# Mimari

paas, her `git push`'u değişmez bir **deployment**'a çeviren, Git tabanlı bir PaaS'tır.
Bu belge sistemi dışarıdan içeriye doğru anlatır: bağlam, konteynerler, iki temel akış
(deploy ve rollback), durum makinesi ve veri modeli.

## 1. Bağlam

Sistemin dünyayla ilişkisi: geliştirici koda push eder, son kullanıcı uygulamayı açar,
platform GitHub ve AWS ile konuşur.

```mermaid
flowchart LR
    dev([Geliştirici])
    user([Son kullanıcı])
    gh[GitHub<br/>repo + webhook + API]
    paas[[paas<br/>Git tabanlı PaaS]]
    aws[AWS<br/>EC2 · Route 53 · ECR]
    le[Let's Encrypt]

    dev -- "git push" --> gh
    gh -- "push / pull_request webhook<br/>(HMAC imzalı)" --> paas
    paas -- "clone (SHA), commit status,<br/>PR yorumu" --> gh
    dev -- "web arayüzü / API:<br/>loglar, rollback, env" --> paas
    user -- "https://app.domain<br/>https://sha7-app.domain" --> paas
    paas -- "imaj push/pull, DNS-01 kaydı" --> aws
    paas -- "wildcard sertifika (DNS-01)" --> le
```

## 2. Konteynerler

Tek bir EC2 (ARM, k3s) üzerinde çalışan parçalar. Kontrol düzlemi tek bir Go ikilisidir;
API, webhook alıcısı, worker'lar, yönlendirme senkronu ve temizlik döngüsü aynı süreçtedir.

```mermaid
flowchart LR
    gh[GitHub]
    ecr[(ECR<br/>imaj registry)]
    subgraph k3s["k3s kümesi · EC2 t4g.medium (arm64)"]
        direction LR
        traefik["Traefik<br/>ingress + TLS"]
        cm["cert-manager<br/>wildcard *.domain"]
        subgraph ns_paas["namespace: paas"]
            cp["Kontrol düzlemi (Go)<br/>API · webhook · worker<br/>routing · cleanup · web UI"]
            pg[("PostgreSQL<br/>durum · kuyruk · loglar")]
            bk["buildkitd<br/>(rootless)"]
        end
        subgraph ns_app["namespace: app-&lt;app&gt;"]
            ing["Ingress'ler<br/>deploy URL'leri + alias'lar"]
            dep["Deployment + Service<br/>d-&lt;sha7&gt; · commit başına bir"]
        end
    end

    gh -- webhook --> traefik
    traefik --> cp
    traefik --> ing --> dep
    cm -. sertifika .-> traefik
    cp <-- "SQL · LISTEN/NOTIFY" --> pg
    cp -- "buildctl" --> bk
    bk -- "git fetch (SHA)" --> gh
    bk -- "imaj push" --> ecr
    cp -- "Kubernetes API" --> ns_app
    ecr -. "imaj pull" .-> dep
```

| Bileşen | Sorumluluk | Kod |
|---|---|---|
| API + webhook | REST API, imzalı GitHub webhook'u, SSE log akışı | `internal/api`, `internal/webhook` |
| Web arayüzü | Oturum, uygulama/deploy sayfaları, rollback, env | `internal/web`, `internal/auth` |
| Kuyruk + worker | `FOR UPDATE SKIP LOCKED` ile iş alma, heartbeat, aşamalar | `internal/store`, `internal/worker` |
| Build | SHA ile clone, dil algılama, Dockerfile üretimi, BuildKit | `internal/build` |
| Deploy | Namespace, kota, Secret, Deployment, Service, Ingress, hazır olma bekleme | `internal/deploy` |
| Yönlendirme | Alias'ları veritabanından Ingress'lere senkronlama, uzlaştırma | `internal/routing` |
| Temizlik | Saklama politikası, emekliye ayırma, nesne silme | `internal/cleanup` |
| GitHub | Commit status, PR "Preview hazır" yorumu | `internal/github` |
| Altyapı | VPC, EC2, EIP, Route 53, ECR, IAM, cloud-init | `infra/terraform`, `infra/k8s` |

## 3. Akış: push → deploy

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub
    participant API as API / webhook
    participant DB as PostgreSQL
    participant W as Worker
    participant BK as buildkitd
    participant REG as Registry (ECR)
    participant K as Kubernetes

    GH->>API: POST /webhooks/github (push, HMAC imzalı)
    API->>API: imzayı doğrula, repo → app
    API->>DB: INSERT deployment (queued)<br/>(app, commit) benzersiz → tekrar teslimat idempotent
    API-->>GH: 201
    W->>DB: ClaimNext: FOR UPDATE SKIP LOCKED → building
    W-->>GH: commit status: pending
    loop her StaleAfter/4
        W->>DB: heartbeat
    end
    W->>BK: git fetch --depth=1 <sha>, algıla, build
    BK->>REG: push <app>:<sha>
    BK-->>W: digest (sha256:…)
    W->>DB: image = <app>:<sha>@<digest>, status = deploying
    W->>K: Namespace, kota, Secret, Deployment, Service, Ingress d-<sha7>
    K-->>W: readiness (CrashLoop / ImagePull → hemen failed)
    W->>DB: MarkReady + alias'lar (yalnızca ileri gider)
    W->>K: alias Ingress'leri senkronla
    W-->>GH: commit status: success + PR yorumu
    Note over DB,API: Her log satırı NOTIFY üretir,<br/>SSE istemcileri canlı izler
```

## 4. Akış: rollback

Rollback yeni bir build ya da pod yeniden başlatması değildir: yalnızca production
alias'ının işaret ettiği deployment değişir.

```mermaid
sequenceDiagram
    participant U as Geliştirici
    participant API as API / web UI
    participant DB as PostgreSQL
    participant R as Routing
    participant K as Kubernetes (Ingress)

    U->>API: POST /api/apps/blog/rollback {deployment_id}
    API->>DB: hedef ready mi? (FOR SHARE: temizlik onu silemez)
    API->>DB: production alias → hedef
    API->>R: SyncApp(blog)
    R->>DB: alias'ları oku (uygulama başına kilit içinde)
    R->>K: Ingress alias-blog backend → d-<eski sha7>
    API-->>U: 200 (yönlendirme hatasında 502, uzlaştırma döngüsü uygular)
```

Ölçülen API gecikmesi (veritabanı + senkron, dry-run): p50 7 ms, p95 23 ms
([MEASUREMENTS.md](MEASUREMENTS.md)).

## 5. Deployment durum makinesi

```mermaid
stateDiagram-v2
    [*] --> queued: webhook (push)
    queued --> building: worker iş alır
    building --> deploying: imaj push'landı
    deploying --> ready: pod hazır, alias'lar taşındı
    building --> failed: build hatası / zaman aşımı
    deploying --> failed: crash loop, imaj çekilemedi, kota
    building --> queued: worker öldü (heartbeat yok), deneme < 2
    deploying --> queued: worker öldü, deneme < 2
    building --> failed: worker öldü, deneme = 2
    ready --> retired: saklama politikası / branch silindi
    deploying --> retired: branch deploy sırasında silindi
    retired --> queued: aynı commit tekrar push'landı
    failed --> [*]
    retired --> [*]
```

## 6. Veri modeli

```mermaid
erDiagram
    apps ||--o{ deployments : "commit başına bir"
    apps ||--o{ aliases : ""
    apps ||--o{ app_env : ""
    deployments ||--o{ aliases : "hedef"
    deployments ||--o{ deployment_logs : ""

    apps {
        bigint id PK
        text name UK "DNS etiketi"
        text repo_full_name UK "owner/repo"
        text production_branch
    }
    deployments {
        bigint id PK
        bigint app_id FK
        text commit_sha "UK (app_id, commit_sha)"
        text branch
        text status "queued|building|deploying|ready|failed|retired"
        text image "registry/app:sha@sha256:…"
        int attempts
        timestamptz heartbeat_at
        timestamptz retired_at
        timestamptz cleaned_at
    }
    aliases {
        bigint id PK
        text hostname UK
        text kind "production|preview"
        text branch
        bigint deployment_id FK
    }
    deployment_logs {
        bigint id PK "SSE event id"
        bigint deployment_id FK
        text line
    }
    app_env {
        bigint app_id FK
        text key
        text value
    }
```

## 7. Adlandırma

Tüm host'lar tek etiketli ve `*.domain` altındadır; tek bir wildcard sertifika hepsini kapsar.

| Ne | Host | Kubernetes adı |
|---|---|---|
| Deployment (değişmez) | `<sha7>-<app>.<domain>` | `app-<app>/d-<sha7>` |
| Production alias | `<app>.<domain>` | `app-<app>/alias-<app>` |
| Preview alias | `<branch>-<app>.<domain>` | `app-<app>/alias-<branch>-<app>` |
| Kontrol düzlemi | `<domain>` | `paas/paas` |
