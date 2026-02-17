FROM golang:1.23-alpine AS builder

# Adding build tools to the builder stage for Go compilation
RUN apk add --no-cache build-base git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cc-bot .

# --- Using Python Slim (Debian) ---
FROM python:3.11-slim

# Install system dependencies
# - iputils-ping: Provides the ICMP ping tool
# - build-essential: Standard compiler/tools for Go/C extensions
# - git, jq: Standard dev tools
RUN apt-get update && apt-get install -y \
    ca-certificates \
    nodejs \
    npm \
    ffmpeg \
    curl \
    iputils-ping \
    build-essential \
    git \
    openssh-client \
    lynx \
    sqlite3 \
    vim \
    procps \
    jq \
    && curl -sSL https://ngrok-agent.s3.amazonaws.com/ngrok.asc \
       | tee /etc/apt/trusted.gpg.d/ngrok.asc >/dev/null \
    && echo "deb https://ngrok-agent.s3.amazonaws.com bookworm main" \
       | tee /etc/apt/sources.list.d/ngrok.list \
    && apt-get update \
    && apt-get install -y ngrok \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
RUN pip install --no-cache-dir setuptools openai-whisper

# Install Node dependencies
RUN npm install -g @anthropic-ai/claude-code

# Create a non-root user
RUN useradd -m -s /bin/bash bot

WORKDIR /home/bot
COPY --from=builder /app/cc-bot .
RUN chown -R bot:bot /home/bot

USER bot
CMD ["./cc-bot"]