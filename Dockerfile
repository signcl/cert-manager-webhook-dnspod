FROM golang:1.13-buster AS build_deps
WORKDIR /workspace
ENV GO111MODULE=on
RUN apt install git
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o webhook -ldflags '-w -extldflags "-static"' .

FROM ubuntu:18.04
RUN apt update && apt-get -y install ca-certificates
COPY --from=0 /workspace/webhook /usr/local/bin/webhook
ENTRYPOINT ["webhook"]
