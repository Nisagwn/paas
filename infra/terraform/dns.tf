# <domain> → control plane (API + GitHub webhook)
# With dns_provider = "duckdns" the name is pointed at the Elastic IP outside
# Terraform (DuckDNS update API, see infra/README.md); DuckDNS answers every
# sub-subdomain with the same address, which covers the wildcard.
resource "aws_route53_record" "apex" {
  count   = local.route53 ? 1 : 0
  zone_id = var.hosted_zone_id
  name    = var.domain
  type    = "A"
  ttl     = 300
  records = [aws_eip.node.public_ip]
}

# *.<domain> → every deployment / alias URL (<sha7>-<app>, <app>, <branch>-<app>)
resource "aws_route53_record" "wildcard" {
  count   = local.route53 ? 1 : 0
  zone_id = var.hosted_zone_id
  name    = "*.${var.domain}"
  type    = "A"
  ttl     = 300
  records = [aws_eip.node.public_ip]
}
