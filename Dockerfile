FROM golang:1.22-bookworm AS build
WORKDIR /src
ARG GOPROXY
ENV GOPROXY=${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY VERSION ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
  -ldflags "-X github.com/iman-langaran/AdobeConnect-file-recorder/internal/version.Version=$VERSION" \
  -o /out/ac-rec ./cmd/ac-rec

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ac-rec /usr/local/bin/ac-rec
EXPOSE 8080
CMD ["ac-rec", "serve"]
