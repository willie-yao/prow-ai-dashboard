# Builds the orka-gcs-tool shim from the backend module.
# Build context MUST be the backend/ directory:
#   docker build -f experimental/orka/Dockerfile -t orka-gcs-tool-spike:latest backend/
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -o /out/gcs-tool ./cmd/orka-gcs-tool-spike

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gcs-tool /gcs-tool
EXPOSE 8080
ENTRYPOINT ["/gcs-tool"]
