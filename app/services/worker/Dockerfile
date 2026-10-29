FROM golang:1.22

WORKDIR /app

# worker has no dependencies, so there is no go.sum to copy
COPY go.mod ./

RUN go mod download

COPY *.go ./

EXPOSE 8090

RUN CGO_ENABLED=0 GOOS=linux go build -o /worker

CMD ["/worker"]
