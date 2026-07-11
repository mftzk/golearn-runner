FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go build -o /out/golearn-runner .

FROM golang:1.23-bookworm
RUN useradd --create-home --shell /usr/sbin/nologin sandbox
USER sandbox
WORKDIR /home/sandbox
COPY --from=build /out/golearn-runner /usr/local/bin/golearn-runner
ENV PORT=3000
EXPOSE 3000
ENTRYPOINT ["/usr/local/bin/golearn-runner"]
