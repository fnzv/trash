variable "telegram_bot_token" {
  type      = string
  sensitive = true
}

variable "allowed_chat_ids" {
  type = string
}

variable "git_ssh_key" {
  type    = string
  default = ""
}

variable "git_user_name" {
  type    = string
  default = ""
}


variable "gitlab_token" {
  type    = string
  default = ""
}

variable "git_user_email" {
  type    = string
  default = ""
}

variable "ngrok_authtoken" {
  type    = string
  default = ""
}

variable "allowed_tools" {
  type    = string
  default = "Bash(docker *),Bash(apt *),Bash(dpkg *)"
}

variable "skip_permissions" {
  type    = string
  default = "true"
}

# --- 1. SECRET ---
resource "kubernetes_secret_v1" "cc_bot_secret" {
  metadata {
    name = "trash-bot-secrets"
  }
  data = {
    TELEGRAM_BOT_TOKEN = var.telegram_bot_token
    GIT_SSH_KEY = var.git_ssh_key
    GITLAB_TOKEN = var.gitlab_token
    GIT_USER_NAME = var.git_user_name
    GIT_USER_EMAIL = var.git_user_email
    ALLOWED_CHAT_IDS   = var.allowed_chat_ids
    ALLOWED_TOOLS   = var.allowed_tools
    SKIP_PERMISSIONS = var.skip_permissions
    NGROK_AUTHTOKEN = var.ngrok_authtoken
  }
}

# --- 3. DEPLOYMENT ---
resource "kubernetes_deployment_v1" "cc_bot" {
  metadata {
    name = "trash-bot"
    labels = {
      appname = "trash-bot"
    }
  }

  spec {
    replicas = 1

    strategy {
      type = "Recreate"
    }

    selector {
      match_labels = {
        appname = "trash-bot"
      }
    }

    template {
      metadata {
        labels = {
          appname = "trash-bot"
        }
      }

      spec {
        image_pull_secrets {
          name = "gitlab-registry"
        }

        container {
          image             = "registry.gitlab.com/xfrn-lab/trash-bot:latest"
          image_pull_policy = "Always"
          name              = "trash-bot"

          env_from {
            secret_ref {
              name = kubernetes_secret_v1.cc_bot_secret.metadata[0].name
            }
          }

          env {
            name  = "WORK_DIR"
            value = "/home/bot"
          }

          env {
            name  = "CLAUDE_PATH"
            value = "claude"
          }

          env {
            name  = "COMMAND_TIMEOUT"
            value = "5m"
          }

          resources {
            requests = {
              cpu    = "500m"
              memory = "500Mi"
            }
            limits = {
              cpu    = "2000m"
              memory: "2Gi"
            }
          }
        }

      }
    }
  }
}
