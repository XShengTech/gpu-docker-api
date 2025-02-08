FROM golang:1.23.6-alpine AS builder
LABEL stage=gobuilder \
      mainatiner=https://github.com/XShengTech/gpu-docker-api

# RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk add gcc g++ make libffi-dev openssl-dev libtool git

ENV CGO_ENABLED 0
# ENV GOPROXY https://goproxy.cn,direct

WORKDIR /build

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN BRANCH=$(git rev-parse --abbrev-ref HEAD) && \
    VERSION=$(git describe --tags | cut -d'-' -f1) && \
    COMMIT=$(git rev-parse --short HEAD) && \
    GO_VERSION=$(go env GOVERSION) && \
    BUILD_TIME=$(date +%FT%T%z) && \
    go build -ldflags="-s -w -X main.BRANCH=${BRANCH} -X main.VERSION=${VERSION} -X main.COMMIT=${COMMIT} -X main.GoVersion=${GO_VERSION} -X main.BuildTime=${BUILD_TIME}" -trimpath -o gpu-docker-api cmd/gpu-docker-api/main.go

FROM ubuntu:22.04

VOLUME /data
WORKDIR /data

COPY --from=builder /build/gpu-docker-api /data/gpu-docker-api

EXPOSE 2378

ENTRYPOINT ["./gpu-docker-api"]