# check=skip=InvalidDefaultArgInFrom

# Required version arguments are supplied by Make/Compose, without fallback pins.
ARG GO_VERSION
ARG GO_ALPINE_VERSION
ARG ALPINE_VERSION
FROM golang:${GO_VERSION}-alpine${GO_ALPINE_VERSION} AS build
WORKDIR /src
RUN apk add --no-cache make
COPY go.mod go.sum Makefile versions.mk ./
RUN go mod download
RUN make install-proto
COPY . .
RUN CGO_ENABLED=0 make build

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates && addgroup -g 10001 app && adduser -D -u 10001 -G app app
COPY --from=build /src/.bin/registration /usr/local/bin/registration
USER app
ENTRYPOINT ["registration"]
CMD ["serve"]
