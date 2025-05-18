BINARY = gpu-docker-api
GOARCH = amd64
PLATFORM = linux/amd64

BRANCH = $(shell git rev-parse --abbrev-ref HEAD)
VERSION = $(shell git describe --tags | cut -d'-' -f1)
COMMIT = $(shell git rev-parse --short HEAD)
GO_VERSION = $(shell go env GOVERSION)
BUILD_TIME = $(shell date +%FT%T%z)

GITHUB_USER = mayooot
CURRENT_DIR = $(shell pwd)
BUILD_DIR = ${CURRENT_DIR}/cmd/${BINARY}
BIN_DIR= ${CURRENT_DIR}/bin

NVIDIA = nvidia
MOCK = mock

LDFLAGS = -ldflags "-s -w -X main.BRANCH=${BRANCH} -X main.VERSION=${VERSION} -X main.COMMIT=${COMMIT} -X main.GoVersion=${GO_VERSION} -X main.BuildTime=${BUILD_TIME}"

all: fmt imports clean linux darwin windows

build: clean linux darwin windows

nvidia_linux:
	cd ${BUILD_DIR}; \
	GOOS=linux GOARCH=${GOARCH} go build -trimpath ${LDFLAGS} -tags "${NVIDIA}" -o ${BIN_DIR}/${BINARY}-${NVIDIA}-linux-${GOARCH} . ; \
	cd - >/dev/null

nvidia_linux_no_ldflags:
	cd ${BUILD_DIR}; \
	GOOS=linux GOARCH=${GOARCH} go build -trimpath -tags "${NVIDIA}" -o ${BIN_DIR}/${BINARY}-${NVIDIA}-linux-${GOARCH} . ; \
	cd - >/dev/null

nvidia_darwin:
	cd ${BUILD_DIR}; \
	GOOS=darwin GOARCH=${GOARCH} go build -trimpath ${LDFLAGS} -tags "${NVIDIA}" -o ${BIN_DIR}/${BINARY}-${NVIDIA}-darwin-${GOARCH} . ; \
	cd - >/dev/null

nvidia_windows:
	cd ${BUILD_DIR}; \
	GOOS=windows GOARCH=${GOARCH} go build -trimpath ${LDFLAGS} -tags "${NVIDIA}" -o ${BIN_DIR}/${BINARY}-${NVIDIA}-windows-${GOARCH}.exe . ; \
	cd - >/dev/null

mock_linux:
	cd ${BUILD_DIR}; \
	GOOS=linux GOARCH=${GOARCH} go build -trimpath ${LDFLAGS} -tags "${MOCK}" -o ${BIN_DIR}/${BINARY}-${MOCK}-linux-${GOARCH} . ; \
	cd - >/dev/null

docker_build:
	docker build --platform ${PLATFORM} -t ${GITHUB_USER}/${BINARY}:${VERSION} .

docker_push:
	docker push ${GITHUB_USER}/${BINARY}:${VERSION}

clean:
	- rm -f ${BIN_DIR}/*

fmt:
	gofmt -l -w .

imports:
	goimports-reviser --rm-unused -local github.com/${GITHUB_USER}/${BINARY} -format ./...

check:
	golangci-lint run ./...

.PHONY: all build linux linux_no_ldflags darwin windows docker_build docker_push clean fmt imports check