# minipaas altyapısı (Faz 6)

Git tabanlı PaaS'ın AWS üzerindeki altyapısı: `terraform apply` ile boş bir AWS
hesabından çalışan bir platforma.

```
infra/
├── terraform/                  AWS kaynakları + node bootstrap
│   ├── versions.tf             Terraform / provider sürümleri
│   ├── variables.tf            girdiler
│   ├── network.tf              VPC, public subnet, IGW, security group
│   ├── compute.tf              Ubuntu 24.04 arm64 AMI, EC2, Elastic IP, cloud-init
│   ├── iam.tf                  instance role: ECR push/pull, Route 53 DNS-01
│   ├── dns.tf                  <domain> ve *.<domain> → Elastic IP
│   ├── ecr.tf                  kontrol düzlemi reposu + create-on-push şablonu
│   ├── outputs.tf
│   ├── terraform.tfvars.example
│   └── cloud-init/
│       ├── cloud-config.yaml.tftpl     cloud-init (templatefile)
│       ├── bootstrap.sh                k3s → Helm → cert-manager → secret'lar → manifest'ler
│       ├── ecr-auth.sh                 buildctl için ECR config.json yenileyici
│       ├── minipaas-ecr-auth.{service,timer}
│       └── credential-provider.yaml    kubelet ECR credential provider
└── k8s/                        küme manifest'leri (${PLACEHOLDER}'lı)
    ├── 00-namespace.yaml
    ├── 10-postgres.yaml        StatefulSet + Service (local-path PVC)
    ├── 20-buildkitd.yaml       rootless BuildKit Deployment + Service + PVC
    ├── 30-control-plane-rbac.yaml  ServiceAccount + ClusterRole + binding
    ├── 31-control-plane.yaml   Deployment + Service + IngressRoute (<domain>)
    ├── 40-tls.yaml             ClusterIssuer (Route 53 DNS-01), wildcard Certificate,
    │                           Traefik varsayılan TLSStore, HTTP→HTTPS yönlendirme
    └── 50-networkpolicy.yaml   Postgres ve buildkitd'ye yalnızca kontrol düzlemi erişir
```

## Mimari

```
                    Route 53:  <domain>, *.<domain>  ──►  Elastic IP
                                                            │
┌─ VPC 10.0.0.0/16 ── public subnet ─────────────────────────┼──────────────┐
│  EC2 t4g.medium (Graviton, arm64) · Ubuntu 24.04 · k3s     ▼              │
│                                                                           │
│  kube-system   Traefik (:80 → :443 yönlendirme, :443 TLS)                 │
│                └─ varsayılan sertifika: wildcard-tls (<domain>, *.<domain>)│
│  cert-manager  controller (hostNetwork) ── DNS-01 ──► Route 53            │
│  minipaas      minipaas (API + webhook + worker'lar)                      │
│                  ├─ buildctl ──tcp:1234──► buildkitd (rootless) ──push──► ECR
│                  ├─ Postgres StatefulSet (local-path PVC)                 │
│                  └─ Kubernetes API ──► app-<ad> namespace'leri            │
│  app-*         kullanıcı uygulamaları (kubelet imajları ECR'den çeker)    │
│                                                                           │
│  host: kubelet ecr-credential-provider · minipaas-ecr-auth.timer (6 sa)   │
└───────────────────────────────────────────────────────────────────────────┘
```

| Host adı | Hedef |
|---|---|
| `https://<domain>` | Kontrol düzlemi API'si ve `POST /webhooks/github` |
| `https://<sha7>-<app>.<domain>` | Tek bir commit'in deployment'ı |
| `https://<app>.<domain>`, `https://<branch>-<app>.<domain>` | Production / preview alias'ları |

API bilerek apex alan adında: tüm tek etiketli alt alan adları (`<app>.<domain>`)
uygulamalara ait olduğundan `api.<domain>`, `api` adlı bir uygulamayla çakışırdı.

### Tasarım kararları

**Kimlik bilgileri yalnızca host ağında.** Kümedeki tek AWS kimliği EC2 instance
role'üdür. IMDS yalnızca IMDSv2'ye izin verir ve hop limiti 1'dir; bu yüzden rol
host ağ namespace'indeki süreçlerden (kubelet, systemd, `hostNetwork` pod'lar)
alınabilir, pod ağındaki pod'lardan (yani kullanıcı uygulamalarından) alınamaz.
Kullanıcı kodu ECR'ye push edemez, Route 53'ü değiştiremez, user-data'yı okuyamaz.

**Uygulama pod'larının ECR'den imaj çekmesi — kubelet credential provider.**
k3s, `/var/lib/rancher/credentialprovider/{bin,config.yaml}` varsa kubelet'in
image credential provider özelliğini açar. Bootstrap, Kubernetes'in resmi
`ecr-credential-provider` ikilisini (cloud-provider-aws, arm64) kurar.
`*.dkr.ecr.*.amazonaws.com` imajları için kubelet bu ikiliyi host üzerinde
çalıştırır; ikili instance role ile 12 saatlik token alır ve önbelleğe koyar.
Sonuç: deployer `imagePullSecrets` yönetmez, token süresi dolmaz, k3s'i yeniden
başlatmak gerekmez (statik `registries.yaml` yaklaşımında token 12 saatte biter
ve her yenilemede k3s restart ister).

**buildctl'in ECR'ye push etmesi — systemd timer + Secret.** buildctl, kayıt
kimliklerini istemci tarafında `$DOCKER_CONFIG/config.json`'dan okuyup oturum
üzerinden buildkitd'ye iletir. Host'taki `minipaas-ecr-auth.timer` her 6 saatte
bir (token ömrü 12 saat) `aws ecr get-login-password` ile `config.json` üretir ve
`minipaas/ecr-docker-config` Secret'ına yazar. Kontrol düzlemi bu Secret'ı
`subPath` olmadan `/docker-config`'e bağlar (`DOCKER_CONFIG=/docker-config`), böylece
yenilemeler yeniden başlatma olmadan pod'a yansır. Kontrol düzlemi imajına ek ikili
(ör. `docker-credential-ecr-login`) gerekmez, pod'un AWS kimliğine ihtiyacı olmaz.
buildkitd kayıt sırrı tutmaz.

**Uygulama başına ECR reposu — create-on-push.** Builder imajları
`<hesap>.dkr.ecr.<bölge>.amazonaws.com/minipaas/<app>:<sha>` adresine gönderir.
`aws_ecr_repository_creation_template` (`CREATE_ON_PUSH`, önek `minipaas`) sayesinde
`minipaas/<app>` reposu ilk push'ta, şablondaki yaşam döngüsü kuralıyla (son 30
imaj) otomatik oluşur. Kontrol düzlemi ECR API'sini hiç çağırmaz. Instance role,
yalnızca `repository/minipaas/*` üzerinde push/pull + `CreateRepository` yetkisine
sahiptir.

**Wildcard TLS.** cert-manager, Route 53 DNS-01 ile `<domain>` ve `*.<domain>`
için tek sertifika alır (ClusterIssuer, access key olmadan "ambient credentials").
cert-manager controller'ı instance role'e erişebilmek için `hostNetwork` ile
çalışır (chart'ta bu ayar olmadığından bootstrap `kubectl patch` uygular). IAM
politikası yalnızca o zone'daki `_acme-challenge.<domain>` TXT kayıtlarına
izin verir. Sertifika Traefik'in **varsayılan TLSStore**'una bağlıdır: uygulama
IngressRoute'larında `tls: {}` yazılır, ayrı bir secret referansı gerekmez.

**Secret'lar node üzerinde üretilir.** Postgres parolası, API token'ı ve webhook
secret'ı bootstrap sırasında `openssl rand` ile üretilip doğrudan Kubernetes
Secret'ı olarak oluşturulur. Terraform state'inde ve EC2 user-data'sında yer almaz.

**Manifest'ler ve yer tutucular.** `k8s/*.yaml` dosyalarındaki `${DOMAIN}` gibi
yer tutucular Terraform `templatefile()` ile render edilir (bkz. `compute.tf`,
`local.k8s_vars`; bilinmeyen bir yer tutucu `plan` aşamasında hata verir) ve
cloud-init ile node'a yazılır. Aynı dosyalar gün-2 değişiklikleri için `envsubst`
ile de render edilebilir (aşağıda).

**Rootless BuildKit.** `moby/buildkit:v0.20.2-rootless`, ayrıcalıksız pod olarak
(`--oci-worker-no-process-sandbox`, seccomp/AppArmor `Unconfined`) çalışır. Ubuntu
24.04, AppArmor ile ayrıcalıksız user namespace'leri kısıtladığından bootstrap
`kernel.apparmor_restrict_unprivileged_userns=0` ayarlar. buildkitd'ye TLS'siz TCP
ile bağlanılır; NetworkPolicy yalnızca kontrol düzlemi pod'una izin verir.

## Ön koşullar

- Terraform ≥ 1.6, AWS CLI v2, Docker (buildx ile), `kubectl` (isteğe bağlı)
- AWS hesabı ve yönetici yetkili kimlik bilgileri (`aws configure` / `AWS_PROFILE`)
- Route 53'te bir hosted zone (ör. `example.com`); platform alan adı bu zone'un
  kendisi ya da alt alan adı olabilir (ör. `paas.example.com`)
- Bir SSH anahtar çifti (`ssh-keygen -t ed25519`)
- Kendi genel IP'niz: `curl -s https://checkip.amazonaws.com`

## Kurulum

```bash
cd infra/terraform
cp terraform.tfvars.example terraform.tfvars   # değerleri doldurun
terraform init
terraform plan -out tfplan
terraform apply tfplan
```

`apply` birkaç dakikada biter; node bootstrap'i (k3s, cert-manager, manifest'ler)
arka planda 5–10 dakika daha sürer. İzlemek için:

```bash
$(terraform output -raw ssh) sudo tail -f /var/log/minipaas-bootstrap.log
```

İlk kurulumda `letsencrypt_environment = "staging"` önerilir (Let's Encrypt rate
limitlerine takılmamak için). Her şey çalışınca `"production"` yapıp ClusterIssuer'ı
yeniden uygulayın (bkz. "Gün-2 değişiklikleri") ve sertifikayı yenileyin:
`kubectl -n kube-system delete secret wildcard-tls`.

### kubeconfig

```bash
eval "$(terraform output -raw kubeconfig_command)"
export KUBECONFIG=$PWD/kubeconfig-minipaas.yaml
kubectl get pods -A
kubectl -n kube-system get certificate wildcard     # READY=True olmalı
```

6443 portu yalnızca `admin_cidrs`'ten erişilebilir.

## Kontrol düzlemi imajını göndermek

Node arm64 olduğu için imaj `linux/arm64` olmalıdır. Depo kökünde:

```bash
REPO=$(terraform -chdir=infra/terraform output -raw control_plane_repository_url)
REGISTRY=$(terraform -chdir=infra/terraform output -raw ecr_registry)
aws ecr get-login-password --region eu-central-1 \
  | docker login --username AWS --password-stdin "$REGISTRY"

docker buildx build --platform linux/arm64 -t "$REPO:latest" --push .
kubectl -n minipaas rollout restart deployment/minipaas
```

İmaj gönderilene kadar `minipaas` pod'u `ImagePullBackOff` durumunda bekler; bu
beklenen bir durumdur, imaj gelince kendiliğinden toparlanır. Sabit sürüm için
`control_plane_image_tag` değişkenini (ör. git SHA) kullanın.

> `MINIPAAS_DEPLOYER=kubernetes` Faz 3'teki Kubernetes deployer'ını gerektirir.
> O sürümden eski bir imajla `control_plane_deployer = "dryrun"` kullanın.

## Secret'lar

```bash
# API token
kubectl -n minipaas get secret minipaas -o go-template='{{index .data "api-token" | base64decode}}'
# GitHub webhook secret
kubectl -n minipaas get secret minipaas -o go-template='{{index .data "github-webhook-secret" | base64decode}}'

# Özel repolar için GitHub token'ı (fine-grained, Contents: read)
kubectl -n minipaas patch secret minipaas -p '{"stringData":{"github-token":"github_pat_..."}}'
kubectl -n minipaas rollout restart deployment/minipaas
```

## GitHub webhook'u

Repo → Settings → Webhooks → Add webhook:

- **Payload URL:** `terraform output -raw github_webhook_url` → `https://<domain>/webhooks/github`
- **Content type:** `application/json`
- **Secret:** yukarıdaki `github-webhook-secret`
- **SSL verification:** Enable (staging sertifikasıyla geçici olarak Disable gerekir)
- **Events:** Just the push event

Uygulamayı kaydetmek:

```bash
TOKEN=$(kubectl -n minipaas get secret minipaas -o go-template='{{index .data "api-token" | base64decode}}')
curl -s -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"blog","repo":"<kullanıcı>/blog"}' https://<domain>/api/apps
```

## Gün-2 değişiklikleri

EC2 kaynağı `user_data` ve `ami` değişikliklerini yok sayar (Postgres verisi kök
diskte durduğu için node'un yeniden yaratılmasını istemeyiz). Manifest
değişikliklerini doğrudan kümeye uygulayın; aynı yer tutucular `envsubst` ile
render edilir:

```bash
cd infra/terraform
eval "$(terraform output -raw k8s_env)"
VARS='${DOMAIN} ${AWS_REGION} ${HOSTED_ZONE_ID} ${LETSENCRYPT_EMAIL} ${ACME_SERVER} ${MINIPAAS_REGISTRY} ${CONTROL_PLANE_IMAGE} ${MINIPAAS_DEPLOYER}'
for f in ../k8s/*.yaml; do envsubst "$VARS" <"$f"; echo '---'; done | kubectl apply -f -
```

`cert-manager` için `helm upgrade` yaparsanız controller'ın `hostNetwork` yaması
silinir; node'da `sudo /usr/local/sbin/minipaas-bootstrap` komutunu tekrar
çalıştırın (betik idempotenttir).

## Maliyet tahmini (eu-central-1, on-demand, aylık ~730 saat)

| Kalem | Hesap | Aylık |
|---|---|---|
| EC2 t4g.medium | ~0,0384 $/saat | ~28 $ |
| EBS gp3 40 GiB | ~0,095 $/GiB | ~3,8 $ |
| Genel IPv4 adresi (Elastic IP) | 0,005 $/saat | ~3,7 $ |
| Route 53 hosted zone + sorgular | 0,50 $ + ~0,40 $/milyon sorgu | ~0,5–1 $ |
| ECR depolama | 0,10 $/GB | ~0,5–2 $ |
| Veri çıkışı | ilk 100 GB/ay ücretsiz, sonra ~0,09 $/GB | ~0 $ |
| **Toplam** | | **~37–40 $/ay** |

Notlar: `t4g` instance'ı `unlimited` kredi modunda çalışır; uzun süre %20 temel
CPU seviyesinin üzerinde kalan build'ler vCPU-saat başına ~0,04 $ ek ücret doğurabilir.
NAT gateway ve load balancer kullanılmadığı için bunların (~35 $ + ~20 $/ay) maliyeti
yoktur. 1 yıllık Savings Plan ile EC2 kalemi ~%30–40 düşer. Fiyatlar bölgeye göre
değişir; güncel değerler için AWS Pricing Calculator'a bakın.

## Kaldırma

Create-on-push ile oluşan uygulama repoları Terraform state'inde değildir; önce
onları silin, sonra altyapıyı kaldırın:

```bash
REGION=eu-central-1
for r in $(aws ecr describe-repositories --region $REGION \
    --query "repositories[?starts_with(repositoryName, 'minipaas/')].repositoryName" --output text); do
  aws ecr delete-repository --region $REGION --force --repository-name "$r"
done

cd infra/terraform
terraform destroy
```

`terraform destroy` node'u, kök diski (Postgres verisi dahil), Elastic IP'yi, DNS
kayıtlarını ve kontrol düzlemi ECR reposunu (`force_delete`) siler. Gerekirse önce
yedek alın:

```bash
kubectl -n minipaas exec postgres-0 -- pg_dump -U minipaas minipaas > minipaas.sql
```

## Bilinen kısıtlar ve sonraki adımlar

- **Durum node'un kök diskinde.** Postgres ve BuildKit önbelleği k3s `local-path`
  volume'lerinde. Node kaybı veri kaybıdır; EBS snapshot (DLM) veya RDS'e geçiş ve
  çok düğümlü kurulum için ayrı bir veri diski planlanmalı.
- **BuildKit registry cache ECR'de kapalı** (`MINIPAAS_BUILD_CACHE=false`). ECR,
  cache export için `image-manifest=true,oci-mediatypes=true` ister; builder bu
  seçenekleri eklediğinde açılabilir. Yerel önbellek (PVC) çalışmaya devam eder.
- **Geniş ClusterRole.** Kontrol düzlemi tüm namespace'lerde Secret/Deployment
  yönetebilir. İleride admission policy ile `app-*` namespace'lerine daraltılabilir.
- **Uygulama izolasyonu.** `app-*` namespace'leri için default-deny NetworkPolicy
  deployer'ın (Faz 3) sorumluluğundadır; altyapı yalnızca `minipaas` namespace'ini
  korur.
