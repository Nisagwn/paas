data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720475219"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

resource "aws_key_pair" "admin" {
  key_name   = "${var.name}-admin"
  public_key = var.ssh_public_key
}

# Allocated separately from the instance so its address can be baked into the
# k3s API certificate (--tls-san) through user data.
resource "aws_eip" "node" {
  domain = "vpc"
  tags   = { Name = var.name }

  depends_on = [aws_internet_gateway.main]
}

locals {
  # route53: one wildcard certificate (DNS-01, 41-tls-wildcard.yaml).
  # duckdns: one certificate per host (HTTP-01), no wildcard manifest.
  route53 = var.dns_provider == "route53"

  acme_server = {
    staging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
    production = "https://acme-v02.api.letsencrypt.org/directory"
  }[var.letsencrypt_environment]

  # Every ${PLACEHOLDER} used in ../k8s/*.yaml. templatefile() fails on an
  # unknown placeholder, so this map is the single source of truth. The same
  # files can be rendered by envsubst for day-2 changes (output k8s_env).
  k8s_vars = {
    DOMAIN              = var.domain
    AWS_REGION          = var.region
    HOSTED_ZONE_ID      = var.hosted_zone_id
    LETSENCRYPT_EMAIL   = var.letsencrypt_email
    ACME_SERVER         = local.acme_server
    PAAS_REGISTRY       = "${local.ecr_registry}/${var.ecr_app_prefix}"
    CONTROL_PLANE_IMAGE = "${aws_ecr_repository.control_plane.repository_url}:${var.control_plane_image_tag}"
    PAAS_DEPLOYER       = var.control_plane_deployer
    # The control plane's own host and, without a wildcard, every app host.
    APEX_ISSUER         = local.route53 ? "letsencrypt" : "letsencrypt-http01"
    INGRESS_CERT_ISSUER = local.route53 ? "" : "letsencrypt-http01"
  }

  k8s_dir   = "${path.module}/../k8s"
  manifests = [for f in sort(fileset(local.k8s_dir, "*.yaml")) : f if local.route53 || f != "41-tls-wildcard.yaml"]

  # Non-secret settings for the bootstrap scripts. Secrets (DB password, API
  # token, webhook secret) are generated on the node and never pass through
  # Terraform state or user data.
  bootstrap_env = {
    AWS_REGION                      = var.region
    DOMAIN                          = var.domain
    PUBLIC_IP                       = aws_eip.node.public_ip
    ECR_REGISTRY                    = local.ecr_registry
    K3S_VERSION                     = var.k3s_version
    CERT_MANAGER_VERSION            = var.cert_manager_version
    ECR_CREDENTIAL_PROVIDER_VERSION = var.ecr_credential_provider_version
  }

  write_files = concat(
    [
      {
        path        = "/etc/paas/bootstrap.env"
        permissions = "0644"
        content     = join("", [for k, v in local.bootstrap_env : "${k}='${v}'\n"])
      },
      {
        path        = "/usr/local/sbin/paas-bootstrap"
        permissions = "0755"
        content     = file("${path.module}/cloud-init/bootstrap.sh")
      },
      {
        path        = "/usr/local/sbin/paas-ecr-auth"
        permissions = "0755"
        content     = file("${path.module}/cloud-init/ecr-auth.sh")
      },
      {
        path        = "/etc/systemd/system/paas-ecr-auth.service"
        permissions = "0644"
        content     = file("${path.module}/cloud-init/paas-ecr-auth.service")
      },
      {
        path        = "/etc/systemd/system/paas-ecr-auth.timer"
        permissions = "0644"
        content     = file("${path.module}/cloud-init/paas-ecr-auth.timer")
      },
      {
        path        = "/var/lib/rancher/credentialprovider/config.yaml"
        permissions = "0644"
        content     = file("${path.module}/cloud-init/credential-provider.yaml")
      },
    ],
    [for f in local.manifests : {
      path        = "/opt/paas/manifests/${f}"
      permissions = "0644"
      content     = templatefile("${local.k8s_dir}/${f}", local.k8s_vars)
    }],
  )
}

data "cloudinit_config" "node" {
  gzip          = true
  base64_encode = true

  part {
    content_type = "text/cloud-config"
    content = templatefile("${path.module}/cloud-init/cloud-config.yaml.tftpl", {
      write_files = local.write_files
    })
  }
}

resource "aws_instance" "node" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.node.id]
  key_name               = aws_key_pair.admin.key_name
  iam_instance_profile   = aws_iam_instance_profile.node.name
  user_data_base64       = data.cloudinit_config.node.rendered

  # IMDSv2 only, and a hop limit of 1: the instance role is reachable from the
  # host network namespace (kubelet, systemd, hostNetwork pods) but not from
  # pods on the pod network, i.e. not from user apps.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_size
    encrypted             = true
    delete_on_termination = true
  }

  credit_specification {
    cpu_credits = "unlimited"
  }

  tags = { Name = var.name }

  lifecycle {
    # A newer AMI or edited manifests must not replace the node (Postgres data
    # lives on its root volume). Day-2 manifest changes: see README.
    ignore_changes = [ami, user_data_base64]
  }
}

resource "aws_eip_association" "node" {
  allocation_id = aws_eip.node.id
  instance_id   = aws_instance.node.id
}
