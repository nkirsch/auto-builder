# deploy.yml passes these from app.env.
ARG HUGO_VERSION=0.147.3
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /builder ./cmd/builder

# The site: whatever lands in /public is served at / (SITE_DIR, set by deploy.yml).
FROM hugomods/hugo:exts-${HUGO_VERSION} AS site
ARG SERVE_SITE=true
WORKDIR /src
COPY . .
RUN if [ "$SERVE_SITE" = true ]; then hugo --minify --destination /public; else mkdir -p /public; fi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /builder /builder
COPY --from=site /public /site
ENTRYPOINT ["/builder"]
