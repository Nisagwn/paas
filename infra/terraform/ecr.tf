data "aws_caller_identity" "current" {}

locals {
  account_id   = data.aws_caller_identity.current.account_id
  ecr_registry = "${local.account_id}.dkr.ecr.${var.region}.amazonaws.com"

  ecr_lifecycle_policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the newest ${var.ecr_keep_images} images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = var.ecr_keep_images
      }
      action = { type = "expire" }
    }]
  })
}

# The control plane's own image. Pushed by the operator (see README).
resource "aws_ecr_repository" "control_plane" {
  name                 = "${var.name}-control-plane"
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "control_plane" {
  repository = aws_ecr_repository.control_plane.name
  policy     = local.ecr_lifecycle_policy
}

# App images: the builder pushes <registry>/<prefix>/<app>:<sha> (and
# :buildcache). ECR creates <prefix>/<app> on the first push from this
# template, so the control plane never calls the ECR API itself.
# Those repositories are NOT in Terraform state; see README "Teardown".
resource "aws_ecr_repository_creation_template" "apps" {
  prefix               = var.ecr_app_prefix
  description          = "paas app images (created on push)"
  applied_for          = ["CREATE_ON_PUSH"]
  image_tag_mutability = "MUTABLE" # :buildcache is overwritten on every build
  lifecycle_policy     = local.ecr_lifecycle_policy

  encryption_configuration {
    encryption_type = "AES256"
  }

  resource_tags = {
    Project   = var.name
    ManagedBy = "ecr-create-on-push"
  }
}
