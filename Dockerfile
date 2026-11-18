# GoReleaser builds this image and supplies the prebuilt binary for each platform.
# Runs as root because the runner needs the Docker socket and the root-only /etc/backproof.
FROM gcr.io/distroless/static-debian13@sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/backproof /usr/bin/backproof

ENTRYPOINT ["/usr/bin/backproof"]
