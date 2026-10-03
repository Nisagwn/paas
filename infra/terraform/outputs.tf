output "public_ip" {
  description = "Elastic IP of the node."
  value       = aws_eip.node.public_ip
}

output "ssh" {
  description = "SSH into the node."
  value       = "ssh ubuntu@${aws_eip.node.public_ip}"
}

output "kubeconfig_command" {
  description = "Fetch a kubeconfig that talks to the node's public IP (6443 is open to admin_cidrs only)."
  value       = "ssh ubuntu@${aws_eip.node.public_ip} sudo cat /etc/rancher/k3s/k3s.yaml | sed 's/127.0.0.1/${aws_eip.node.public_ip}/' > kubeconfig-minipaas.yaml"
}

output "api_url" {
  description = "Control plane API."
  value       = "https://${var.domain}"
}

output "github_webhook_url" {
  description = "Payload URL for the GitHub webhook."
  value       = "https://${var.domain}/webhooks/github"
}

output "ecr_registry" {
  description = "ECR registry host (docker login target)."
  value       = local.ecr_registry
}

output "minipaas_registry" {
  description = "MINIPAAS_REGISTRY: app images go to <this>/<app>:<sha>."
  value       = local.k8s_vars.MINIPAAS_REGISTRY
}

output "control_plane_repository_url" {
  description = "Push the control-plane image here (linux/arm64)."
  value       = aws_ecr_repository.control_plane.repository_url
}

output "k8s_env" {
  description = "Exports for rendering ../k8s/*.yaml with envsubst (day-2 changes)."
  value       = join("\n", [for k, v in local.k8s_vars : "export ${k}='${v}'"])
}
