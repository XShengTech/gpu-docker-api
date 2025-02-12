FROM golang:1.23.6-alpine AS builder
LABEL stage=gobuilder \
      mainatiner=https://github.com/XShengTech/gpu-docker-api

# RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk add gcc g++ make libffi-dev openssl-dev libtool git

ENV CGO_ENABLED=0
# ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /build

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN make linux

FROM ubuntu:22.04

VOLUME /data
WORKDIR /data

COPY --from=builder /build/bin/gpu-docker-api-linux-amd64 /data/gpu-docker-api

EXPOSE 2378

ENTRYPOINT ["./gpu-docker-api"]