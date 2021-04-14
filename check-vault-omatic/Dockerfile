FROM golang:1.15 as builder

RUN useradd -m -d /home/check_vault_omatic -s /bin/bash check_vault_omatic

COPY go.mod /
RUN go mod download

COPY main.go .
RUN GO111MODULE=on CGO_ENABLED=0 GOOS=linux go build -o /app main.go

FROM bash:latest
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --chown=check_vault_omatic --from=builder /app /app
RUN mkdir /home/check_vault_omatic; chown check_vault_omatic /home/check_vault_omatic

USER check_vault_omatic

ENTRYPOINT ["/app"]
