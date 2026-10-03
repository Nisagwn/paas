# The node's instance role is the only AWS identity in the cluster. It is used
# by processes that run in the host network namespace:
#   - kubelet's ecr-credential-provider (pulls app and control-plane images)
#   - the minipaas-ecr-auth systemd timer (docker config.json for buildctl)
#   - the cert-manager controller (hostNetwork; Route 53 DNS-01)
# IMDS is IMDSv2-only with a hop limit of 1 (see compute.tf), so pods on the
# pod network (= every user app) cannot obtain these credentials.

data "aws_iam_policy_document" "assume_ec2" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${var.name}-node"
  assume_role_policy = data.aws_iam_policy_document.assume_ec2.json
}

resource "aws_iam_instance_profile" "node" {
  name = "${var.name}-node"
  role = aws_iam_role.node.name
}

locals {
  app_repos_arn     = "arn:aws:ecr:${var.region}:${local.account_id}:repository/${var.ecr_app_prefix}/*"
  acme_record_names = ["_acme-challenge.${var.domain}"]
}

data "aws_iam_policy_document" "ecr" {
  statement {
    sid       = "EcrLogin"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid = "AppImagesPushPull"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage",
      "ecr:DescribeImages",
      "ecr:DescribeRepositories",
      "ecr:ListImages",
      # Create-on-push: the pushing principal creates the repository and
      # applies the template's lifecycle policy and tags.
      "ecr:CreateRepository",
      "ecr:PutLifecyclePolicy",
      "ecr:TagResource",
    ]
    resources = [local.app_repos_arn]
  }

  statement {
    sid = "ControlPlanePull"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ]
    resources = [aws_ecr_repository.control_plane.arn]
  }
}

data "aws_iam_policy_document" "route53" {
  statement {
    sid       = "AcmeChangeStatus"
    actions   = ["route53:GetChange"]
    resources = ["arn:aws:route53:::change/*"]
  }

  # Only TXT records named _acme-challenge.<domain> (used for both <domain>
  # and *.<domain>) in this one zone.
  statement {
    sid       = "AcmeTxtRecords"
    actions   = ["route53:ChangeResourceRecordSets"]
    resources = ["arn:aws:route53:::hostedzone/${var.hosted_zone_id}"]

    condition {
      test     = "ForAllValues:StringEquals"
      variable = "route53:ChangeResourceRecordSetsNormalizedRecordNames"
      values   = local.acme_record_names
    }
    condition {
      test     = "ForAllValues:StringEquals"
      variable = "route53:ChangeResourceRecordSetsRecordTypes"
      values   = ["TXT"]
    }
  }

  statement {
    sid       = "AcmeListRecords"
    actions   = ["route53:ListResourceRecordSets"]
    resources = ["arn:aws:route53:::hostedzone/${var.hosted_zone_id}"]
  }

  statement {
    sid       = "AcmeFindZone"
    actions   = ["route53:ListHostedZonesByName"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ecr" {
  name   = "ecr"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.ecr.json
}

resource "aws_iam_role_policy" "route53" {
  name   = "route53-dns01"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.route53.json
}
