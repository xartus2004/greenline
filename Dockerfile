# Build: docker build -t greenline .
# Run:   docker run -p 8080:8080 -p 9093:9093 -v gl:/data greenline \
#          serve -repo /data/repo.git -init-from /src/examples/mono -state /data/state -grpc :9093
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/greenline ./cmd/greenline

FROM debian:bookworm-slim
# git for the localgit forge; the build tool runs actions with /bin/sh and coreutils.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 10001 greenline && mkdir /data && chown greenline /data
COPY --from=build /out/greenline /usr/local/bin/greenline
COPY --from=build /src/examples /src/examples
USER greenline
WORKDIR /data
EXPOSE 8080 9092 9093
ENTRYPOINT ["greenline"]
CMD ["serve", "-repo", "/data/repo.git", "-init-from", "/src/examples/mono", "-state", "/data/state", "-addr", ":8080", "-grpc", ":9093"]
