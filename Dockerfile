# A remote ka daemon (PRD §15, §18), built from source:
#   docker build -t ka .
#   docker run -v ka-data:/data ka token create --actor you          # prints a token
#   docker run -d -p 7711:7711 -v ka-data:/data -v ~/.ssh:/home/ka/.ssh:ro ka \
#     daemon --listen 0.0.0.0:7711 --public-url https://ka.example.com --workspace /data/workspace.yml
# See docs/operations.md.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ka ./cmd/ka

FROM alpine:3.22
RUN apk add --no-cache git openssh-client ca-certificates tzdata \
 && adduser -D -u 10001 -h /home/ka ka \
 && mkdir -p /data && chown ka:ka /data
COPY --from=build /out/ka /usr/local/bin/ka
USER ka
ENV KA_DATA_DIR=/data/stores KA_CONFIG_DIR=/data/config KA_RUNTIME_DIR=/data/run HOME=/home/ka
VOLUME ["/data"]
EXPOSE 7711
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:7711/healthz >/dev/null || exit 1
ENTRYPOINT ["ka"]
CMD ["daemon", "--listen", "0.0.0.0:7711"]
