FROM golang:1.22-bookworm AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN go test ./...
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ewelink-lan-ctl ./cmd/ewelink-lan-ctl
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ewelink-lan-ctl /ewelink-lan-ctl
COPY --chown=nonroot:nonroot --from=build /out/data /data

USER nonroot:nonroot
EXPOSE 33998

ENTRYPOINT ["/ewelink-lan-ctl"]
