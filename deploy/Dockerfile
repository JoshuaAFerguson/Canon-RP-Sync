FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /rpsync ./cmd/rpsync

FROM alpine:3.20
COPY --from=build /rpsync /usr/local/bin/rpsync
VOLUME /photos
# host networking is required for SSDP discovery
ENTRYPOINT ["rpsync"]
CMD ["daemon", "--dest", "/photos"]
