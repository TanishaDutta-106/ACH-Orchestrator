variable "aws_region" {
  description = "AWS region to deploy into"
  type        = string
  default     = "us-east-1"
}

variable "project_name" {
  description = "Prefix applied to every resource name and tag"
  type        = string
  default     = "ach-orchestrator"
}

variable "environment" {
  description = "Deployment environment label (dev / staging / prod)"
  type        = string
  default     = "dev"
}

variable "temporal_address" {
  description = "Temporal server gRPC address (host:port). Use Temporal Cloud or self-hosted EC2."
  type        = string
  # For Temporal Cloud free tier: <namespace>.tmprl.cloud:7233
  # For self-hosted:              temporal.internal:7233
  default = "temporal.internal:7233"
}
