variable "name" {
  description = "Name prefix for every AWS resource."
  type        = string
  default     = "paas"
}

variable "region" {
  description = "AWS region. t4g (Graviton) must be available there."
  type        = string
  default     = "eu-central-1"
}

variable "domain" {
  description = "Platform base domain, e.g. \"paas.example.com\". Apps get <app>.<domain>, the API is served at the apex <domain>."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]{2,}$", var.domain))
    error_message = "domain must be a lowercase DNS name without a trailing dot."
  }
}

variable "dns_provider" {
  description = "\"route53\": records in var.hosted_zone_id and one wildcard certificate (DNS-01). \"duckdns\": a DuckDNS name (e.g. \"you.duckdns.org\") pointed at the Elastic IP after apply, and one certificate per host (HTTP-01)."
  type        = string
  default     = "route53"

  validation {
    condition     = contains(["route53", "duckdns"], var.dns_provider)
    error_message = "dns_provider must be \"route53\" or \"duckdns\"."
  }
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone that contains var.domain (the zone of the domain itself or of a parent, e.g. example.com). Only for dns_provider = \"route53\"."
  type        = string
  default     = ""

  validation {
    condition     = var.dns_provider != "route53" || var.hosted_zone_id != ""
    error_message = "hosted_zone_id is required when dns_provider is \"route53\"."
  }
}

variable "letsencrypt_email" {
  description = "Contact e-mail for the Let's Encrypt ACME account."
  type        = string
}

variable "letsencrypt_environment" {
  description = "\"staging\" (untrusted certs, generous rate limits; use while testing) or \"production\"."
  type        = string
  default     = "production"

  validation {
    condition     = contains(["staging", "production"], var.letsencrypt_environment)
    error_message = "letsencrypt_environment must be \"staging\" or \"production\"."
  }
}

variable "ssh_public_key" {
  description = "OpenSSH public key installed for the \"ubuntu\" user."
  type        = string
}

variable "admin_cidrs" {
  description = "CIDRs allowed to reach SSH (22) and the Kubernetes API (6443), e.g. [\"203.0.113.7/32\"]."
  type        = list(string)

  validation {
    condition     = length(var.admin_cidrs) > 0 && alltrue([for c in var.admin_cidrs : can(cidrhost(c, 0))])
    error_message = "admin_cidrs must be a non-empty list of valid CIDRs."
  }
}

variable "availability_zone" {
  description = "AZ of the subnet (and the node). Empty: the region's first. Set another one when AWS reports InsufficientInstanceCapacity for instance_type there."
  type        = string
  default     = ""
}

variable "instance_type" {
  description = "EC2 instance type. Must be ARM64 (Graviton): the AMI and the build platform are arm64."
  type        = string
  default     = "t4g.medium"
}

variable "root_volume_size" {
  description = "Root EBS volume size in GiB. Holds k3s, container images, the BuildKit cache and the Postgres volume."
  type        = number
  default     = 40
}

variable "vpc_cidr" {
  description = "VPC CIDR. Must not overlap the k3s pod (10.42.0.0/16) and service (10.43.0.0/16) CIDRs."
  type        = string
  default     = "10.0.0.0/16"
}

variable "k3s_version" {
  description = "Pinned k3s release (https://github.com/k3s-io/k3s/releases)."
  type        = string
  default     = "v1.36.5+k3s1"
}

variable "cert_manager_version" {
  description = "cert-manager Helm chart version."
  type        = string
  default     = "v1.21.2"
}

variable "ecr_credential_provider_version" {
  description = "Version of the kubelet ecr-credential-provider binary (kubernetes/cloud-provider-aws), served from artifacts.k8s.io."
  type        = string
  default     = "v1.37.0"
}

variable "ecr_app_prefix" {
  description = "ECR namespace for app images. Repositories <prefix>/<app> are created on first push from a repository creation template."
  type        = string
  default     = "paas"
}

variable "ecr_keep_images" {
  description = "Lifecycle policy: images kept per repository (oldest are expired first)."
  type        = number
  default     = 30
}

variable "control_plane_image_tag" {
  description = "Tag of the control-plane image in the <name>-control-plane ECR repository."
  type        = string
  default     = "latest"
}

variable "control_plane_deployer" {
  description = "PAAS_DEPLOYER for the control plane. \"kubernetes\" needs the Faz 3 deployer; use \"dryrun\" with older images."
  type        = string
  default     = "kubernetes"
}
