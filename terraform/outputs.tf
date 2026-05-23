output "alb_dns_name" {
  description = "Public DNS of the Application Load Balancer"
  value       = aws_lb.main.dns_name
}

output "ecr_repository_url" {
  description = "ECR repository URL — use this in the GitHub Actions deploy workflow"
  value       = aws_ecr_repository.app.repository_url
}

output "rds_endpoint" {
  description = "RDS PostgreSQL endpoint (host:port)"
  value       = aws_db_instance.postgres.endpoint
}

output "redis_endpoint" {
  description = "ElastiCache Redis endpoint"
  value       = "${aws_elasticache_cluster.redis.cache_nodes[0].address}:6379"
}

output "teardown_command" {
  description = "One-command teardown — stops ALL charges immediately"
  value       = "terraform destroy -auto-approve"
}
