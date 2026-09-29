FROM golang:1.22-bookworm AS build

WORKDIR /src

ARG TASK_VERSION=3.53.1
ARG TARGETOS
ARG TARGETARCH

ADD "https://github.com/go-task/task/releases/download/v${TASK_VERSION}/task_linux_${TARGETARCH}.tar.gz" /tmp/task.tar.gz
RUN tar -xzf /tmp/task.tar.gz -C /usr/local/bin task

COPY go.mod Taskfile.yml ./
RUN task deps

COPY . .
RUN task container GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" OUTPUT=/out/ewelink-lan-ctl
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ewelink-lan-ctl /ewelink-lan-ctl
COPY --chown=nonroot:nonroot --from=build /out/data /data

USER nonroot:nonroot
EXPOSE 33998

ENTRYPOINT ["/ewelink-lan-ctl"]
