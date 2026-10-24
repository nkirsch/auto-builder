FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /builder ./cmd/builder

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /builder /builder
ENTRYPOINT ["/builder"]
