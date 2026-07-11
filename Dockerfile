# Single-stage: the final image must keep the full Go toolchain because the
# sandbox compiles and runs user code with `go run` at request time.
FROM golang:1.23-bookworm
WORKDIR /src
COPY go.mod ./
COPY main.go ./
# GOTOOLCHAIN=local pins to the image's Go 1.23 and forbids downloading a
# newer toolchain — the Nrapken builder has no outbound network, so any
# toolchain/module fetch would fail the build.
ENV GOCACHE=/tmp/gocache GOPATH=/tmp/gopath HOME=/tmp GOTOOLCHAIN=local
RUN go build -o /usr/local/bin/golearn-runner .
ENV PORT=3000
EXPOSE 3000
USER nobody
ENTRYPOINT ["/usr/local/bin/golearn-runner"]
