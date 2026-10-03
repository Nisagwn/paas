# <domain> → control plane (API + GitHub webhook)
resource "aws_route53_record" "apex" {
  zone_id = var.hosted_zone_id
  name    = var.domain
  type    = "A"
  ttl     = 300
  records = [aws_eip.node.public_ip]
}

# *.<domain> → every deployment / alias URL (<sha7>-<app>, <app>, <branch>-<app>)
resource "aws_route53_record" "wildcard" {
  zone_id = var.hosted_zone_id
  name    = "*.${var.domain}"
  type    = "A"
  ttl     = 300
  records = [aws_eip.node.public_ip]
}
