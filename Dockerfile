FROM golang:1.23-alpine AS builder
RUN apk add --no-cache build-base git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cc-bot .

FROM python:3.11-slim

# Combine System Deps, Ngrok, and Node to reduce intermediate layers
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates nodejs npm ffmpeg curl iputils-ping git \
    openssh-client sqlite3 procps jq \
    && curl -sSL https://ngrok-agent.s3.amazonaws.com/ngrok.asc \
       | tee /etc/apt/trusted.gpg.d/ngrok.asc >/dev/null \
    && echo "deb https://ngrok-agent.s3.amazonaws.com bookworm main" \
       | tee /etc/apt/sources.list.d/ngrok.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends ngrok \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install CPU-only PyTorch + Whisper + Claude Code in one layer to save space during build
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu && \
    pip install --no-cache-dir openai-whisper && \
    npm install -g @anthropic-ai/claude-code && \
    npm cache clean --force

RUN useradd -m -s /bin/bash bot
WORKDIR /home/bot
COPY --from=builder /app/cc-bot .
RUN chown -R bot:bot /home/bot

USER bot
CMD ["./cc-bot"]