# check=skip=InvalidDefaultArgInFrom

# Required version arguments are supplied by Make/Compose, without fallback pins.
ARG GO_VERSION
ARG GO_ALPINE_VERSION
ARG ALPINE_VERSION
FROM golang:${GO_VERSION}-alpine${GO_ALPINE_VERSION} AS build
WORKDIR /src
RUN apk add --no-cache make curl unzip gcompat libstdc++
COPY go.mod go.sum Makefile versions.mk ./
RUN go mod download
RUN version="$(make -s versions | awk -F= '$1 == "PROTOC_VERSION" {print $2}')"; \
    case "$(uname -m)" in x86_64) arch=x86_64 ;; aarch64) arch=aarch_64 ;; *) exit 1 ;; esac; \
    curl -fsSL --retry 3 "https://github.com/protocolbuffers/protobuf/releases/download/v${version}/protoc-${version}-linux-${arch}.zip" -o /tmp/protoc.zip && \
    unzip -q /tmp/protoc.zip -d /usr/local && rm /tmp/protoc.zip && \
    make install-proto
COPY . .
RUN CGO_ENABLED=0 make build

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates && addgroup -g 10001 app && adduser -D -u 10001 -G app app
COPY --from=build /src/.bin/registration /usr/local/bin/registration
USER app
ENTRYPOINT ["registration"]
CMD ["serve"]
