data "aws_caller_identity" "current" {}

locals {
  account_id   = data.aws_caller_identity.current.account_id
  ecr_registry = "${local.account_id}.dkr.ecr.${var.region}.amazonaws.com"

  # The control plane's own repository: the operator pushes its tags by
  # hand; the newest ecr_keep_images stay.
  control_plane_lifecycle_policy = jsonencode({
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

  # App repositories (Faz 30): no count limit. Every deployment's image has
  # its own tag (<sha>-<deployment id>) and the control plane deletes it
  # when the deployment is retired and no kept deployment (live, rollback
  # target, active preview) references the same digest. A count rule here
  # would expire the image of a rollback target, or even of the live
  # deployment, once enough previews were pushed (ImagePullBackOff on the
  # next reschedule). Only untagged images expire: superseded build caches
  # and the push of a build that crashed before its image was recorded.
  # Children of an image index are never expired while the index exists
  # (ECR rule), so multi-platform and attested images stay whole.
  app_lifecycle_policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images ${var.ecr_untagged_days} days after push"
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = var.ecr_untagged_days
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
  policy     = local.control_plane_lifecycle_policy
}

# App images: the builder pushes <registry>/<prefix>/<app>:<sha>-<id> (and
# :buildcache). ECR creates <prefix>/<app> on the first push from this
# template. The control plane deletes the images of retired deployments
# through the registry API (DELETE /v2/<repo>/manifests/<digest>, which
# needs ecr:BatchDeleteImage) with the token the builder already uses; it
# never calls the ECR control API.
# Those repositories are NOT in Terraform state; see README "Teardown".
# The template applies to repositories created after it changed; see
# infra/README.md "İmaj saklama (Faz 30)" for repositories that already exist.
resource "aws_ecr_repository_creation_template" "apps" {
  prefix               = var.ecr_app_prefix
  description          = "paas app images (created on push)"
  applied_for          = ["CREATE_ON_PUSH"]
  image_tag_mutability = "MUTABLE" # :buildcache is overwritten on every build
  lifecycle_policy     = local.app_lifecycle_policy

  encryption_configuration {
    encryption_type = "AES256"
  }

  resource_tags = {
    Project   = var.name
    ManagedBy = "ecr-create-on-push"
  }
}
