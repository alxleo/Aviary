FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.9.0 AS xx


# Frontend build
FROM --platform=$BUILDPLATFORM node:24-alpine AS ui-builder
WORKDIR /app

COPY ui/package.json ui/package-lock.json ui/
RUN cd ui && npm ci
COPY locales/ locales/
COPY ui/ ui/
RUN cd ui && npm run build


FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.23@sha256:0525c952defebc693cdaf62cdf43afa32be5a7f377e99260642ea19c240c708e AS go-base
WORKDIR /app
COPY --from=xx / /
RUN apk add --no-cache git

# Build the same rmapi release with patched image decoders and JWT parser.
FROM --platform=$BUILDPLATFORM go-base AS rmapi-builder
WORKDIR /src/rmapi
ARG TARGETPLATFORM
RUN git clone --branch v0.0.35 --depth 1 https://github.com/ddvk/rmapi.git . \
    && test "$(git rev-parse HEAD)" = "74a8e2ec7f324655ef3a3890936ed78e6e13ee51" \
    && sed -i 's#"github.com/golang-jwt/jwt"#"github.com/golang-jwt/jwt/v4"#' api/api.go \
    && go get golang.org/x/image@v0.45.0 github.com/golang-jwt/jwt/v4@v4.5.2 \
    && go mod tidy \
    && go test ./api/... \
    && CGO_ENABLED=0 xx-go build -trimpath -ldflags="-w -s" -o /out/rmapi .

# Aviary build
FROM --platform=$BUILDPLATFORM go-base AS aviary-builder

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /app/ui/dist ./ui/dist

# Build args for version injection
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETPLATFORM

RUN --mount=type=cache,target=/root/.cache \
    CGO_ENABLED=0 xx-go build \
    -ldflags="-w -s \
        -X github.com/rmitchellscott/aviary/internal/version.Version=${VERSION} \
        -X github.com/rmitchellscott/aviary/internal/version.GitCommit=${GIT_COMMIT} \
        -X github.com/rmitchellscott/aviary/internal/version.BuildDate=${BUILD_DATE}" \
    -trimpath


# Final image
FROM alpine:3.23

# Install runtime dependencies
RUN apk upgrade --no-cache \
    && apk add --no-cache \
      ca-certificates \
      ghostscript \
      imagemagick \
      postgresql-client \
      mupdf-tools \
    && update-ca-certificates

WORKDIR /app

COPY --from=rmapi-builder /out/rmapi /usr/local/bin/
COPY --from=aviary-builder /app/aviary /usr/local/bin/
COPY --from=aviary-builder /app/locales ./locales

ENV PORT=8000 \
    PDF_DIR=/app/pdfs \
    RM_TARGET_DIR=/ \
    GS_COMPAT=1.7 \
    GS_SETTINGS=/ebook

ENTRYPOINT ["/usr/local/bin/aviary"]
