FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY . .
# Cross-compiled for the target platform, so no emulation is needed.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/taskboard-runner ./cmd/taskboard-runner

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/taskboard-runner /taskboard-runner
USER nonroot:nonroot
ENTRYPOINT ["/taskboard-runner"]
# With no arguments the image is a fleet; `test` and `validate` replace this.
CMD ["run"]
