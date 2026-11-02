# Build: docker build -t greenline .
#   Behind a TLS-intercepting proxy, add its CA without baking it into the image:
#   docker build --secret id=extra_ca,src=/path/to/proxy-ca.crt -t greenline .
# Run:   docker run -p 8080:8080 -p 9093:9093 -v gl:/data greenline \
#          serve -repo /data/repo.git -init-from /src/examples/mono -state /data/state -grpc :9093
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -f /run/secrets/extra_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.crt && export SSL_CERT_FILE=/tmp/ca.crt; \
    fi; \
    go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/greenline ./cmd/greenline

# buildpack-deps:*-scm already ships git, ca-certificates, /bin/sh and
# coreutils (needed by the localgit forge and the build tool), so the image
# builds without package-manager network access.
FROM buildpack-deps:bookworm-scm
RUN useradd -m -u 10001 greenline && mkdir /data && chown greenline /data
COPY --from=build /out/greenline /usr/local/bin/greenline
COPY --from=build /src/examples /src/examples
USER greenline
WORKDIR /data
EXPOSE 8080 9092 9093
ENTRYPOINT ["greenline"]
CMD ["serve", "-repo", "/data/repo.git", "-init-from", "/src/examples/mono", "-state", "/data/state", "-addr", ":8080", "-grpc", ":9093"]
