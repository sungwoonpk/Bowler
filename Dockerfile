# Build stage. The program has no C dependencies, so the result is one static file.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bowler ./cmd/bowler \
 && mkdir /out/data

# Run stage. Distroless has no shell, no package manager and no curl, so there is almost nothing in the image to
# attack. It runs as an unprivileged user, and /data is owned by that user so the database can be written.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bowler /bowler
COPY --from=build --chown=65532:65532 /out/data /data
ENV BOWLER_ADDR=0.0.0.0:8080 \
    BOWLER_DB=/data/bowler.db
VOLUME /data
EXPOSE 8080
USER 65532:65532
# The health check is the program itself, because the image has no curl.
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 CMD ["/bowler", "healthcheck"]
ENTRYPOINT ["/bowler"]
CMD ["serve"]
